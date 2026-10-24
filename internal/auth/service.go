// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// Identity + session domain service. All queries go through sqlc-generated
// db.Queries (Rule 2) — never inline SQL.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pquerna/otp/totp"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
)

var (
	ErrEmailTaken     = errors.New("email_taken")
	ErrBadCredentials = errors.New("invalid_credentials")
	ErrUserSuspended  = errors.New("user_suspended")
	ErrSessionRevoked = errors.New("session_revoked")
	ErrWeakPassword   = errors.New("weak_password")
	ErrMFARequired    = errors.New("mfa_required")
	ErrMFACode        = errors.New("invalid_mfa_code")
	ErrUserNotFound   = errors.New("user_not_found")
)

type User struct {
	ID          uuid.UUID `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	IsVerified  bool      `json:"is_verified"`
	Status      string    `json:"status"`
}

type Service struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	vk        valkey.Client
	jwtSecret string
	tokenTTL  time.Duration
}

func NewService(pool *pgxpool.Pool, vk valkey.Client, jwtSecret string, tokenTTL time.Duration) *Service {
	return &Service{pool: pool, q: db.New(pool), vk: vk, jwtSecret: jwtSecret, tokenTTL: tokenTTL}
}

func toPg(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }
func fromPg(p pgtype.UUID) uuid.UUID {
	u, _ := uuid.FromBytes(p.Bytes[:])
	return u
}
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// ValidatePassword — §02 policy: min 8 chars, at least one letter + one digit.
func ValidatePassword(pw string) error {
	if len(pw) < 8 {
		return ErrWeakPassword
	}
	var letter, digit bool
	for _, r := range pw {
		if unicode.IsLetter(r) {
			letter = true
		}
		if unicode.IsDigit(r) {
			digit = true
		}
	}
	if !letter || !digit {
		return ErrWeakPassword
	}
	return nil
}

// EmitEvent appends to the domain-event outbox (§05). Best-effort until the
// dispatcher lands in Phase 06 — emission must never block the request.
func (s *Service) EmitEvent(ctx context.Context, name, aggType string, aggID uuid.UUID, payload map[string]any) {
	b, _ := json.Marshal(payload)
	_ = s.q.EmitDomainEvent(ctx, db.EmitDomainEventParams{
		Name: name, AggregateType: aggType, AggregateID: toPg(aggID), Payload: b,
	})
}

func (s *Service) Register(ctx context.Context, email, password, displayName string) (*User, string, error) {
	if err := ValidatePassword(password); err != nil {
		return nil, "", err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, "", err
	}
	row, err := s.q.CreateUser(ctx, db.CreateUserParams{
		Email: email, PasswordHash: pgText(hash), DisplayName: displayName,
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, "", ErrEmailTaken
		}
		return nil, "", err // real DB error → 500, never fake email_taken
	}
	u := &User{ID: fromPg(row.ID), Email: row.Email, DisplayName: row.DisplayName, IsVerified: row.IsVerified, Status: row.Status}
	s.EmitEvent(ctx, "UserRegistered", "user", u.ID, map[string]any{"email": u.Email})
	verifyToken, _ := SignActionToken(s.jwtSecret, u.ID.String(), "verify_email", 24*time.Hour)
	return u, verifyToken, nil
}

func (s *Service) Login(ctx context.Context, email, password, mfaCode, deviceLabel, ip, ua string) (string, uuid.UUID, error) {
	row, err := s.q.GetUserByEmail(ctx, email)
	if err == pgx.ErrNoRows {
		return "", uuid.Nil, ErrBadCredentials
	}
	if err != nil {
		return "", uuid.Nil, err // real DB error → 500
	}
	if row.Status != "active" {
		return "", uuid.Nil, ErrUserSuspended
	}
	if !row.PasswordHash.Valid || !CheckPassword(row.PasswordHash.String, password) {
		return "", uuid.Nil, ErrBadCredentials
	}
	// §02: MFA-enabled accounts must present a valid TOTP code at login.
	if row.MfaSecret != "" {
		if mfaCode == "" {
			return "", uuid.Nil, ErrMFARequired
		}
		if !totp.Validate(mfaCode, row.MfaSecret) {
			return "", uuid.Nil, ErrMFACode
		}
	}
	uid := fromPg(row.ID)
	token, sessionID, err := s.createSession(ctx, uid, deviceLabel, ip, ua)
	if err != nil {
		return "", uuid.Nil, err
	}
	s.EmitEvent(ctx, "UserLoggedIn", "user", uid, map[string]any{"provider": "password"})
	return token, sessionID, nil
}

// createSession is the shared session-issuing tail of Login/OAuthCallback:
// insert row → sign JWT keyed to jti → return session id.
func (s *Service) createSession(ctx context.Context, userID uuid.UUID, deviceLabel, ip, ua string) (string, uuid.UUID, error) {
	jti := uuid.New()
	expires := time.Now().Add(s.tokenTTL)
	addr, _ := netip.ParseAddr(ip)
	sessID, err := s.q.CreateSession(ctx, db.CreateSessionParams{
		UserID:      toPg(userID),
		Jti:         toPg(jti),
		DeviceLabel: pgText(deviceLabel),
		Column4:     addr,
		UserAgent:   pgText(ua),
		ExpiresAt:   pgtype.Timestamptz{Time: expires, Valid: true},
	})
	if err != nil {
		return "", uuid.Nil, err
	}
	token, err := IssueToken(s.jwtSecret, userID, jti, s.tokenTTL)
	if err != nil {
		return "", uuid.Nil, err
	}
	return token, fromPg(sessID), nil
}

func (s *Service) Logout(ctx context.Context, jti uuid.UUID) error {
	if row, err := s.q.GetSessionByJTI(ctx, toPg(jti)); err == nil {
		s.EmitEvent(ctx, "SessionRevoked", "session", fromPg(row.ID), map[string]any{"reason": "logout"})
	}
	return s.q.RevokeSessionByJTI(ctx, toPg(jti))
}

// ResolveSession maps a JWT jti → userID, enforcing revocation + expiry (§02).
func (s *Service) ResolveSession(ctx context.Context, jti uuid.UUID) (uuid.UUID, error) {
	row, err := s.q.GetSessionByJTI(ctx, toPg(jti))
	if err == pgx.ErrNoRows {
		return uuid.Nil, ErrSessionRevoked
	}
	if err != nil {
		return uuid.Nil, err
	}
	if row.RevokedAt.Valid || row.ExpiresAt.Time.Before(time.Now()) {
		return uuid.Nil, ErrSessionRevoked
	}
	return fromPg(row.UserID), nil
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	row, err := s.q.GetUserByID(ctx, toPg(id))
	if err == pgx.ErrNoRows {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &User{ID: id, Email: row.Email, DisplayName: row.DisplayName, IsVerified: row.IsVerified, Status: row.Status}, nil
}

func (s *Service) UserIDByEmail(ctx context.Context, email string) (string, error) {
	id, err := s.q.GetUserIDByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	return fromPg(id).String(), nil
}

// VerifyEmail consumes the single-use signed token and marks the user verified.
func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	userID, err := s.ConsumeActionToken(ctx, token, "verify_email")
	if err != nil {
		return err
	}
	uid := uuid.MustParse(userID)
	s.EmitEvent(ctx, "IdentityVerified", "user", uid, map[string]any{"method": "email_link"})
	return s.q.SetUserVerified(ctx, toPg(uid))
}

// ConsumeActionToken verifies a signed token and burns it (single-use,
// oga9:auth:used_token:{jti} SET NX EX).
func (s *Service) ConsumeActionToken(ctx context.Context, token, purpose string) (string, error) {
	jti, userID, err := VerifyActionToken(s.jwtSecret, token, purpose)
	if err != nil {
		return "", err
	}
	ok, err := s.vk.Do(ctx, s.vk.B().Set().Key("oga9:auth:used_token:"+jti).Value("1").Nx().ExSeconds(86400).Build()).AsBool()
	if err != nil || !ok {
		return "", errors.New("token_used")
	}
	return userID, nil
}

func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	userID, err := s.ConsumeActionToken(ctx, token, "password_reset")
	if err != nil {
		return err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	uid := uuid.MustParse(userID)
	if err := s.q.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{PasswordHash: pgText(hash), ID: toPg(uid)}); err != nil {
		return err
	}
	// revoke all sessions on password reset (§02)
	s.EmitEvent(ctx, "PasswordChanged", "user", uid, map[string]any{"via": "reset_link"})
	return s.q.RevokeAllUserSessions(ctx, toPg(uid))
}

func (s *Service) SetupMFA(ctx context.Context, userID uuid.UUID, secret string) error {
	return s.q.SetMFASecret(ctx, db.SetMFASecretParams{MfaSecret: []byte(secret), ID: toPg(userID)})
}

func (s *Service) MFASecret(ctx context.Context, userID uuid.UUID) (string, error) {
	return s.q.GetMFASecret(ctx, toPg(userID))
}

type SessionInfo struct {
	ID          uuid.UUID `json:"id"`
	DeviceLabel string    `json:"device_label"`
	IP          string    `json:"ip"`
	UserAgent   string    `json:"user_agent"`
	CreatedAt   time.Time `json:"created_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

func (s *Service) ListSessions(ctx context.Context, userID uuid.UUID) ([]SessionInfo, error) {
	rows, err := s.q.ListUserSessions(ctx, toPg(userID))
	if err != nil {
		return nil, err
	}
	out := make([]SessionInfo, len(rows))
	for i, r := range rows {
		out[i] = SessionInfo{
			ID:          fromPg(r.ID),
			DeviceLabel: r.DeviceLabel.String,
			IP:          r.Ip,
			UserAgent:   r.UserAgent.String,
			CreatedAt:   r.CreatedAt.Time,
			ExpiresAt:   r.ExpiresAt.Time,
		}
	}
	return out, nil
}

func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	s.EmitEvent(ctx, "SessionRevoked", "session", sessionID, map[string]any{"reason": "revoked_by_user"})
	return s.q.RevokeSessionByID(ctx, db.RevokeSessionByIDParams{ID: toPg(sessionID), UserID: toPg(userID)})
}

func (s *Service) LinkOAuthProvider(ctx context.Context, userID uuid.UUID, provider, subject string) error {
	return s.q.LinkOAuthProvider(ctx, db.LinkOAuthProviderParams{
		UserID: toPg(userID), Provider: provider, ProviderSubject: subject,
	})
}

func (s *Service) userByOAuthSubject(ctx context.Context, provider, subject string) (uuid.UUID, error) {
	id, err := s.q.GetUserByOAuthSubject(ctx, db.GetUserByOAuthSubjectParams{Provider: provider, ProviderSubject: subject})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(id), nil
}

func (s *Service) createOAuthUser(ctx context.Context, email, displayName string, verified bool) (uuid.UUID, error) {
	id, err := s.q.CreateOAuthUser(ctx, db.CreateOAuthUserParams{Email: email, DisplayName: displayName, IsVerified: verified})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(id), nil
}
