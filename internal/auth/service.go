// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// Identity + session domain service. All queries go through sqlc-generated
// db.Queries (Rule 2) — never inline SQL.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
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
	"github.com/jedafe/oga9/internal/events"
)

var (
	ErrEmailTaken       = errors.New("email_taken")
	ErrBadCredentials   = errors.New("invalid_credentials")
	ErrUserSuspended    = errors.New("user_suspended")
	ErrTenantSuspended  = errors.New("tenant_suspended")
	ErrSessionRevoked   = errors.New("session_revoked")
	ErrWeakPassword     = errors.New("weak_password")
	ErrMFARequired      = errors.New("mfa_required")
	ErrMFASetupRequired = errors.New("mfa_setup_required")
	ErrMFAGateInvalid   = errors.New("mfa_gate_invalid")
	ErrMFACode          = errors.New("invalid_mfa_code")
	ErrUserNotFound     = errors.New("user_not_found")
	ErrInvalidEmail     = errors.New("invalid_email")
	ErrEmailNotVerified = errors.New("email_not_verified")
	ErrMFANotEnabled    = errors.New("mfa_not_enabled")
	ErrRegClosed        = errors.New("registration_closed")
)

type User struct {
	ID          uuid.UUID `json:"user_id"`
	Email       string    `json:"email"`
	DisplayName string    `json:"display_name"`
	IsVerified  bool      `json:"is_verified"`
	Status      string    `json:"status"`
}

// LoginResult carries the session on success; on ErrMFARequired /
// ErrMFASetupRequired it carries MFAToken so the client can complete the
// gate via /auth/mfa/complete or /auth/mfa/setup+confirm. On
// ErrEmailNotVerified (OAuth) it carries Email for the resend path.
type LoginResult struct {
	Token     string
	SessionID uuid.UUID
	UserID    uuid.UUID // set on session success — drives post-login hooks
	MFAToken  string
	Email     string
}

type Service struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	vk        valkey.Client
	jwtSecret string
	tokenTTL  time.Duration
	dataKey   []byte
}

func NewService(pool *pgxpool.Pool, vk valkey.Client, jwtSecret string, tokenTTL time.Duration, dataKey []byte) *Service {
	return &Service{pool: pool, q: db.New(pool), vk: vk, jwtSecret: jwtSecret, tokenTTL: tokenTTL, dataKey: dataKey}
}

func toPg(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(p pgtype.UUID) uuid.UUID {
	u, _ := uuid.FromBytes(p.Bytes[:])
	return u
}
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }

// dummyHash keeps unknown-email logins on the bcrypt path so response time
// doesn't disclose account existence.
var dummyHash, _ = HashPassword("oga9-timing-equalizer")

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

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// registrationOpen reads system_settings.platform.registration_open
// (catalog default: true when the key is absent).
func (s *Service) registrationOpen(ctx context.Context) bool {
	v, err := s.q.GetSystemSetting(ctx, "platform.registration_open")
	if err != nil {
		return true
	}
	return strings.TrimSpace(string(v)) == "true"
}

func (s *Service) Register(ctx context.Context, email, password, displayName string) (*User, string, error) {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address == "" {
		return nil, "", ErrInvalidEmail
	}
	email = addr.Address // normalize "Name <a@b>" → "a@b" before it hits the citext column
	if err := ValidatePassword(password); err != nil {
		return nil, "", err
	}
	if !s.registrationOpen(ctx) {
		return nil, "", ErrRegClosed
	}
	hash, err := HashPassword(password)
	if err != nil {
		return nil, "", err
	}
	var row db.CreateUserRow
	err = s.withTx(ctx, func(q *db.Queries) error {
		r, e := q.CreateUser(ctx, db.CreateUserParams{
			Email: email, PasswordHash: pgText(hash), DisplayName: displayName,
		})
		if e != nil {
			return e
		}
		row = r
		return events.Emit(ctx, q, uuid.Nil, uuid.Nil, "UserRegistered", "user", fromPg(r.ID), fromPg(r.ID), false,
			map[string]any{"user_id": fromPg(r.ID), "email": r.Email})
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, "", ErrEmailTaken
		}
		return nil, "", err // real DB error → 500, never fake email_taken
	}
	u := &User{ID: fromPg(row.ID), Email: row.Email, DisplayName: row.DisplayName, IsVerified: row.IsVerified, Status: row.Status}
	verifyToken, _ := SignActionToken(s.jwtSecret, u.ID.String(), "verify_email", 24*time.Hour)
	return u, verifyToken, nil
}

func (s *Service) Login(ctx context.Context, email, password, mfaCode, deviceLabel, ip, ua string) (*LoginResult, error) {
	if addr, err := mail.ParseAddress(email); err == nil {
		email = addr.Address // same normalization as Register
	}
	row, err := s.q.GetUserByEmail(ctx, email)
	if err == pgx.ErrNoRows {
		CheckPassword(dummyHash, password) // keep response time flat
		return nil, ErrBadCredentials
	}
	if err != nil {
		return nil, err // real DB error → 500
	}
	if !row.PasswordHash.Valid || !CheckPassword(row.PasswordHash.String, password) {
		return nil, ErrBadCredentials
	}
	// status + verification checks run only after credentials prove out, so
	// probing without the password can't distinguish account states.
	if row.Status != "active" {
		return nil, ErrUserSuspended
	}
	if !row.IsVerified {
		return nil, ErrEmailNotVerified
	}
	return s.finishLogin(ctx, fromPg(row.ID), row.MfaSecret, mfaCode, deviceLabel, ip, ua, "password")
}

// finishLogin is the shared post-credential tail: MFA gate → session.
// mfaSecret is the raw encrypted bytea; empty means MFA is off.
func (s *Service) finishLogin(ctx context.Context, userID uuid.UUID, mfaSecret []byte, mfaCode, deviceLabel, ip, ua, provider string) (*LoginResult, error) {
	// §15: a suspended tenant blocks login for its users; platform operators
	// (platform.tenants.manage) still authenticate to support/reinstate.
	healthy, err := s.q.UserHasHealthyMembership(ctx, toPg(userID))
	if err == nil && !healthy.Bool {
		op, e := s.q.UserHasAnyPlatformRole(ctx, toPg(userID))
		if e != nil || !op {
			return nil, ErrTenantSuspended // every org they belong to is frozen
		}
	}
	if len(mfaSecret) > 0 {
		if mfaCode == "" {
			tok, err := s.stageMFAGate(ctx, userID, deviceLabel, ip, ua, provider)
			if err != nil {
				return nil, err
			}
			return &LoginResult{MFAToken: tok}, ErrMFARequired
		}
		if err := s.checkMFACode(ctx, userID, mfaSecret, mfaCode); err != nil {
			return nil, err
		}
	} else {
		// §02: platform.* holders and org admins must enroll MFA — gate the
		// login until setup completes rather than issuing a session.
		req, err := s.q.UserRequiresMFA(ctx, toPg(userID))
		if err != nil {
			return nil, err
		}
		if req {
			tok, err := s.stageMFAGate(ctx, userID, deviceLabel, ip, ua, provider)
			if err != nil {
				return nil, err
			}
			return &LoginResult{MFAToken: tok}, ErrMFASetupRequired
		}
	}
	token, sessionID, err := s.createSession(ctx, userID, deviceLabel, ip, ua, provider)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Token: token, SessionID: sessionID, UserID: userID}, nil
}

// createSession is the shared session-issuing tail of Login/OAuthCallback:
// session row + UserLoggedIn event in one tx → sign JWT keyed to jti.
func (s *Service) createSession(ctx context.Context, userID uuid.UUID, deviceLabel, ip, ua, provider string) (string, uuid.UUID, error) {
	return s.createSessionTTL(ctx, userID, deviceLabel, ip, ua, provider, s.tokenTTL, uuid.Nil)
}

// createSessionTTL — same flow with an explicit TTL (§15 support sessions
// run a fixed 30min regardless of the session TTL).
func (s *Service) createSessionTTL(ctx context.Context, userID uuid.UUID, deviceLabel, ip, ua, provider string, ttl time.Duration, supportSessionID uuid.UUID) (string, uuid.UUID, error) {
	jti := uuid.New()
	expires := time.Now().Add(ttl)
	var sessID pgtype.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		id, e := q.CreateSession(ctx, db.CreateSessionParams{
			UserID:           toPg(userID),
			Jti:              toPg(jti),
			DeviceLabel:      pgText(deviceLabel),
			Ip:               ip,
			UserAgent:        pgText(ua),
			ExpiresAt:        pgtype.Timestamptz{Time: expires, Valid: true},
			SupportSessionID: toPg(supportSessionID),
		})
		if e != nil {
			return e
		}
		sessID = id
		return events.Emit(ctx, q, uuid.Nil, uuid.Nil, "UserLoggedIn", "user", userID, userID, false,
			map[string]any{"user_id": userID, "provider": provider})
	})
	if err != nil {
		return "", uuid.Nil, err
	}
	token, err := IssueToken(s.jwtSecret, userID, jti, ttl)
	if err != nil {
		return "", uuid.Nil, err
	}
	return token, fromPg(sessID), nil
}

// Logout revokes the session row; SessionRevoked emits only when a live
// session was actually revoked (idempotent — no phantom events).
func (s *Service) Logout(ctx context.Context, jti uuid.UUID) error {
	err := s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.GetSessionByJTI(ctx, toPg(jti))
		if err != nil {
			return nil // unknown session — logout stays idempotent
		}
		if _, err := q.RevokeSessionByJTI(ctx, toPg(jti)); err != nil {
			if err == pgx.ErrNoRows {
				return nil // already revoked
			}
			return err
		}
		return events.Emit(ctx, q, uuid.Nil, uuid.Nil, "SessionRevoked", "session", fromPg(row.ID), fromPg(row.UserID), false,
			map[string]any{"session_id": fromPg(row.ID), "reason": "logout"})
	})
	if err == nil {
		s.denySession(ctx, jti)
	}
	return err
}

// denySession marks a jti on the revoked-session denylist
// (oga9:auth:deny:{jti}; TTL tracks OGA9_SESSION_TTL so the marker always
// outlives the token it suppresses). The DB row stays the source of truth;
// this is the §02 denylist fast path so revoked tokens skip the join.
func (s *Service) denySession(ctx context.Context, jti uuid.UUID) {
	ttl := max(int64(s.tokenTTL.Seconds()), 60)
	_ = s.vk.Do(ctx, s.vk.B().Set().Key("oga9:auth:deny:"+jti.String()).Value("1").ExSeconds(ttl).Build()).Error()
}

// ResolveSession maps a JWT jti → userID, enforcing revocation + expiry +
// account status (a suspended account's live sessions die immediately).
// The Valkey denylist is checked first — a revoked token never reaches
// Postgres.
func (s *Service) ResolveSession(ctx context.Context, jti uuid.UUID) (uuid.UUID, error) {
	if err := s.vk.Do(ctx, s.vk.B().Get().Key("oga9:auth:deny:"+jti.String()).Build()).Error(); err == nil {
		return uuid.Nil, ErrSessionRevoked
	}
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
	if row.UserStatus != "active" {
		return uuid.Nil, ErrUserSuspended
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
	if addr, err := mail.ParseAddress(email); err == nil {
		email = addr.Address
	}
	id, err := s.q.GetUserIDByEmail(ctx, email)
	if err != nil {
		return "", err
	}
	return fromPg(id).String(), nil
}

// VerifyEmail consumes the single-use signed token, marks the user
// verified, and returns the user — the handler uses the verified email
// to materialize any approved pre-signup applications (§11 hook).
func (s *Service) VerifyEmail(ctx context.Context, token string) (*User, error) {
	jti, userID, err := s.ConsumeActionToken(ctx, token, "verify_email")
	if err != nil {
		return nil, err
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		return nil, errors.New("malformed token subject")
	}
	var u *User
	err = s.withTx(ctx, func(q *db.Queries) error {
		if err := q.SetUserVerified(ctx, toPg(uid)); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, uuid.Nil, uuid.Nil, "IdentityVerified", "user", uid, uid, true,
			map[string]any{"user_id": uid, "method": "email_link"}); err != nil {
			return err
		}
		row, err := q.GetUserByID(ctx, toPg(uid))
		if err != nil {
			return err
		}
		u = &User{ID: uid, Email: row.Email, DisplayName: row.DisplayName, IsVerified: row.IsVerified, Status: row.Status}
		return nil
	})
	if err != nil {
		s.releaseActionToken(ctx, jti) // burned but tx failed — allow retry
		return nil, err
	}
	return u, nil
}

// ConsumeActionToken verifies a signed token and burns it (single-use,
// oga9:auth:used_token:{jti} SET NX EX). Returns the token jti so callers can
// release the burn if the guarded mutation fails.
func (s *Service) ConsumeActionToken(ctx context.Context, token, purpose string) (string, string, error) {
	jti, userID, err := VerifyActionToken(s.jwtSecret, token, purpose)
	if err != nil {
		return "", "", err
	}
	ok, err := s.vk.Do(ctx, s.vk.B().Set().Key("oga9:auth:used_token:"+jti).Value("1").Nx().ExSeconds(86400).Build()).AsBool()
	if err != nil || !ok {
		return "", "", errors.New("token_used")
	}
	return jti, userID, nil
}

// releaseActionToken un-burns a token when the tx it guarded rolled back —
// a transient failure must not strand a still-valid token.
func (s *Service) releaseActionToken(ctx context.Context, jti string) {
	_ = s.vk.Do(ctx, s.vk.B().Del().Key("oga9:auth:used_token:"+jti).Build()).Error()
}

func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	if err := ValidatePassword(newPassword); err != nil {
		return err
	}
	jti, userID, err := s.ConsumeActionToken(ctx, token, "password_reset")
	if err != nil {
		return err
	}
	hash, err := HashPassword(newPassword)
	if err != nil {
		s.releaseActionToken(ctx, jti)
		return err
	}
	uid, err := uuid.Parse(userID)
	if err != nil {
		s.releaseActionToken(ctx, jti)
		return errors.New("malformed token subject")
	}
	var revokedJTIs []pgtype.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		if err := q.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{PasswordHash: pgText(hash), ID: toPg(uid)}); err != nil {
			return err
		}
		jtis, err := q.RevokeAllUserSessions(ctx, toPg(uid))
		if err != nil {
			return err
		}
		revokedJTIs = jtis
		return events.Emit(ctx, q, uuid.Nil, uuid.Nil, "PasswordChanged", "user", uid, uid, false,
			map[string]any{"user_id": uid, "via": "reset_link"})
	})
	if err != nil {
		s.releaseActionToken(ctx, jti)
		return err
	}
	for _, j := range revokedJTIs {
		s.denySession(ctx, fromPg(j))
	}
	return nil
}

// StageMFASecret parks a TOTP secret in Valkey (oga9:mfa:pending:{user_id},
// 10 min) until the user proves it with a valid code — never trust a
// client-supplied secret at confirm time.
func (s *Service) StageMFASecret(ctx context.Context, userID uuid.UUID, secret string) error {
	return s.vk.Do(ctx, s.vk.B().Set().Key("oga9:mfa:pending:"+userID.String()).Value(secret).ExSeconds(600).Build()).Error()
}

func (s *Service) PendingMFASecret(ctx context.Context, userID uuid.UUID) (string, error) {
	return s.vk.Do(ctx, s.vk.B().Get().Key("oga9:mfa:pending:"+userID.String()).Build()).ToString()
}

func (s *Service) ClearPendingMFA(ctx context.Context, userID uuid.UUID) {
	_ = s.vk.Do(ctx, s.vk.B().Del().Key("oga9:mfa:pending:"+userID.String()).Build())
}

// mfaGate is the staged post-credential context carried by an mfa_token
// (oga9:auth:mfa_gate:{nonce}, 10 min) until the caller proves a second factor.
type mfaGate struct {
	UserID      uuid.UUID `json:"user_id"`
	DeviceLabel string    `json:"device_label"`
	IP          string    `json:"ip"`
	UserAgent   string    `json:"user_agent"`
	Provider    string    `json:"provider"`
}

func (s *Service) stageMFAGate(ctx context.Context, userID uuid.UUID, deviceLabel, ip, ua, provider string) (string, error) {
	nonce := uuid.NewString()
	b, _ := json.Marshal(mfaGate{UserID: userID, DeviceLabel: deviceLabel, IP: ip, UserAgent: ua, Provider: provider})
	err := s.vk.Do(ctx, s.vk.B().Set().Key("oga9:auth:mfa_gate:"+nonce).Value(string(b)).ExSeconds(600).Build()).Error()
	if err != nil {
		return "", err
	}
	return nonce, nil
}

// ResolveMFAGate reads a gate nonce without consuming it — mfa/setup and
// mfa/confirm need two requests against the same gate.
func (s *Service) ResolveMFAGate(ctx context.Context, nonce string) (uuid.UUID, error) {
	raw, err := s.vk.Do(ctx, s.vk.B().Get().Key("oga9:auth:mfa_gate:"+nonce).Build()).AsBytes()
	if err != nil {
		return uuid.Nil, ErrMFAGateInvalid
	}
	var g mfaGate
	if err := json.Unmarshal(raw, &g); err != nil || g.UserID == uuid.Nil {
		return uuid.Nil, ErrMFAGateInvalid
	}
	return g.UserID, nil
}

func (s *Service) dropMFAGate(ctx context.Context, nonce string) {
	_ = s.vk.Do(ctx, s.vk.B().Del().Key("oga9:auth:mfa_gate:"+nonce).Build())
}

// DropMFAGate consumes a gate nonce after it served its purpose
// (mfa/confirm finished enrollment for a gated login).
func (s *Service) DropMFAGate(ctx context.Context, nonce string) { s.dropMFAGate(ctx, nonce) }

// ConsumeMFAGate reads + burns a gate nonce atomically (GETDEL) — truly
// single-use; a concurrent complete can't replay the same nonce.
func (s *Service) ConsumeMFAGate(ctx context.Context, nonce string) (mfaGate, error) {
	var g mfaGate
	raw, err := s.vk.Do(ctx, s.vk.B().Getdel().Key("oga9:auth:mfa_gate:"+nonce).Build()).AsBytes()
	if err != nil {
		return g, ErrMFAGateInvalid
	}
	if err := json.Unmarshal(raw, &g); err != nil || g.UserID == uuid.Nil {
		return g, ErrMFAGateInvalid
	}
	return g, nil
}

// CompleteMFALogin finishes a gated login (OAuth or privileged password):
// gate nonce + live TOTP or recovery code → session.
func (s *Service) CompleteMFALogin(ctx context.Context, mfaToken, code string) (*LoginResult, error) {
	gate, err := s.ConsumeMFAGate(ctx, mfaToken)
	if err != nil {
		return nil, err
	}
	row, err := s.q.GetUserAuthState(ctx, toPg(gate.UserID))
	if err != nil {
		return nil, err
	}
	if row.Status != "active" {
		return nil, ErrUserSuspended
	}
	if err := s.checkMFACode(ctx, gate.UserID, row.MfaSecret, code); err != nil {
		return nil, err
	}
	token, sessionID, err := s.createSession(ctx, gate.UserID, gate.DeviceLabel, gate.IP, gate.UserAgent, gate.Provider)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Token: token, SessionID: sessionID, UserID: gate.UserID}, nil
}

// decryptMFA unwraps the AES-GCM envelope; plaintext TOTP never persists.
func (s *Service) decryptMFA(blob []byte) (string, error) {
	b, err := decryptBlob(s.dataKey, blob)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// checkMFACode validates a live TOTP against the encrypted secret, falling
// back to a one-time recovery code (consumed on use).
func (s *Service) checkMFACode(ctx context.Context, userID uuid.UUID, encSecret []byte, code string) error {
	secret, err := s.decryptMFA(encSecret)
	if err != nil {
		return err
	}
	if totp.Validate(code, secret) {
		return nil
	}
	if ok, err := s.ConsumeRecoveryCode(ctx, userID, code); err != nil || !ok {
		return ErrMFACode
	}
	return nil
}

// SetupMFA encrypts + writes the confirmed secret and issues one-time
// recovery codes (sha256(code+userID) hashes only — plaintext shown once).
func (s *Service) SetupMFA(ctx context.Context, userID uuid.UUID, secret string) (codes []string, err error) {
	codes = make([]string, 8)
	hashes := make([]string, 8)
	for i := range codes {
		codes[i] = newRecoveryCode()
		hashes[i] = recoveryHash(codes[i], userID)
	}
	b, _ := json.Marshal(hashes)
	enc, err := encryptBlob(s.dataKey, []byte(secret))
	if err != nil {
		return nil, err
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		if e := q.SetMFASecret(ctx, db.SetMFASecretParams{MfaSecret: enc, ID: toPg(userID)}); e != nil {
			return e
		}
		if e := q.SetRecoveryCodes(ctx, db.SetRecoveryCodesParams{MfaRecoveryCodes: b, ID: toPg(userID)}); e != nil {
			return e
		}
		return events.Emit(ctx, q, uuid.Nil, uuid.Nil, "MFAEnabled", "user", userID, userID, false, map[string]any{"user_id": userID})
	})
	return codes, err
}

// newRecoveryCode — 16 base32 chars (~80 bits) grouped xxxx-xxxx-xxxx-xxxx.
func newRecoveryCode() string {
	raw := make([]byte, 10)
	_, _ = rand.Read(raw)
	s := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw))
	return s[:4] + "-" + s[4:8] + "-" + s[8:12] + "-" + s[12:16]
}

func recoveryHash(code string, userID uuid.UUID) string {
	h := sha256.Sum256([]byte(code + ":" + userID.String()))
	return hex.EncodeToString(h[:])
}

// ConsumeRecoveryCode burns a matching one-time code atomically (single
// UPDATE) — concurrent logins can't both redeem the same code. Returns
// false if no match.
func (s *Service) ConsumeRecoveryCode(ctx context.Context, userID uuid.UUID, code string) (bool, error) {
	_, err := s.q.ConsumeRecoveryCode(ctx, db.ConsumeRecoveryCodeParams{
		ID: toPg(userID), Hash: recoveryHash(code, userID),
	})
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// DisableMFA requires the account password + a live TOTP or recovery code;
// clears secret and all remaining recovery codes in one tx.
func (s *Service) DisableMFA(ctx context.Context, userID uuid.UUID, password, code string) error {
	row, err := s.q.GetUserAuthState(ctx, toPg(userID))
	if err != nil {
		return err
	}
	if len(row.MfaSecret) == 0 {
		return ErrMFANotEnabled
	}
	if !row.PasswordHash.Valid || !CheckPassword(row.PasswordHash.String, password) {
		return ErrBadCredentials
	}
	if err := s.checkMFACode(ctx, userID, row.MfaSecret, code); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.SetMFASecret(ctx, db.SetMFASecretParams{MfaSecret: nil, ID: toPg(userID)}); err != nil {
			return err
		}
		if err := q.SetRecoveryCodes(ctx, db.SetRecoveryCodesParams{MfaRecoveryCodes: []byte("[]"), ID: toPg(userID)}); err != nil {
			return err
		}
		return events.Emit(ctx, q, uuid.Nil, uuid.Nil, "MFADisabled", "user", userID, userID, false, map[string]any{"user_id": userID})
	})
}

// MFAEnabled reports whether the account has a live TOTP secret.
func (s *Service) MFAEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	b, err := s.q.GetMFASecret(ctx, toPg(userID))
	return len(b) > 0, err
}

type SessionInfo struct {
	ID          uuid.UUID `json:"id"`
	JTI         uuid.UUID `json:"-"`
	Current     bool      `json:"current"`
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
			JTI:         fromPg(r.Jti),
			DeviceLabel: r.DeviceLabel.String,
			IP:          r.Ip,
			UserAgent:   r.UserAgent.String,
			CreatedAt:   r.CreatedAt.Time,
			ExpiresAt:   r.ExpiresAt.Time,
		}
	}
	return out, nil
}

// RevokeSession revokes one of the caller's sessions; emits SessionRevoked
// only when a row was actually revoked (no phantom events).
func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) (bool, error) {
	revoked := false
	var jti pgtype.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		j, err := q.RevokeSessionByID(ctx, db.RevokeSessionByIDParams{ID: toPg(sessionID), UserID: toPg(userID)})
		if err != nil {
			if err == pgx.ErrNoRows {
				return nil // nothing to revoke — not an error
			}
			return err
		}
		jti = j
		revoked = true
		return events.Emit(ctx, q, uuid.Nil, uuid.Nil, "SessionRevoked", "session", sessionID, userID, false,
			map[string]any{"session_id": sessionID, "reason": "revoked_by_user"})
	})
	if revoked {
		s.denySession(ctx, fromPg(jti))
	}
	return revoked, err
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

// MintActionToken issues a single-use token on another actor's behalf —
// the §15 provision welcome link uses purpose "password_reset" so the
// existing reset flow consumes it (the owner picks their own password).
func (s *Service) MintActionToken(userID uuid.UUID, purpose string, ttl time.Duration) string {
	tok, _ := SignActionToken(s.jwtSecret, userID.String(), purpose, ttl)
	return tok
}

// CreateSupportSession mints a 30min session for the impersonation target
// — the §15 support flow. The user_sessions row links back to the
// support_sessions record so session listings surface the operator chain.
func (s *Service) CreateSupportSession(ctx context.Context, supportSessionID, targetUserID uuid.UUID, ttl time.Duration, ip, ua string) (string, uuid.UUID, error) {
	return s.createSessionTTL(ctx, targetUserID, "support", ip, ua, "support", ttl, supportSessionID)
}
