// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"
)

type Service struct {
	pool     *pgxpool.Pool
	vk       valkey.Client
	jwtSecret string
	tokenTTL time.Duration
}

func NewService(pool *pgxpool.Pool, vk valkey.Client, jwtSecret string, tokenTTL time.Duration) *Service {
	return &Service{pool: pool, vk: vk, jwtSecret: jwtSecret, tokenTTL: tokenTTL}
}

// ConsumeActionToken verifies a signed token and burns it (single-use,
// oga9:auth:used_token:{jti} SET NX EX).
func (s *Service) ConsumeActionToken(ctx context.Context, token, purpose string) (string, error) {
	jti, userID, err := VerifyActionToken(s.jwtSecret, token, purpose)
	if err != nil { return "", err }
	key := "oga9:auth:used_token:" + jti
	ok, err := s.vk.Do(ctx, s.vk.B().Set().Key(key).Value("1").Nx().ExSeconds(86400).Build()).AsBool()
	if err != nil || !ok { return "", errors.New("token_used") }
	return userID, nil
}

func (s *Service) ResetPassword(ctx context.Context, token, newPassword string) error {
	userID, err := s.ConsumeActionToken(ctx, token, "password_reset")
	if err != nil { return err }
	hash, err := HashPassword(newPassword)
	if err != nil { return err }
	_, err = s.pool.Exec(ctx, `UPDATE users SET password_hash=$1 WHERE id=$2`, hash, userID)
	if err != nil { return err }
	// revoke all sessions on password reset
	_, _ = s.pool.Exec(ctx, `UPDATE user_sessions SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, userID)
	return nil
}

func (s *Service) UserIDByEmail(ctx context.Context, email string) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM users WHERE email=$1`, email).Scan(&id)
	return id, err
}

func (s *Service) SetupMFA(ctx context.Context, userID uuid.UUID, secret string) error {
	_, err := s.pool.Exec(ctx, `UPDATE users SET mfa_secret=$1 WHERE id=$2`, secret, userID)
	return err
}

func (s *Service) HasMFA(ctx context.Context, userID uuid.UUID) (bool, error) {
	var has bool
	err := s.pool.QueryRow(ctx, `SELECT mfa_secret IS NOT NULL FROM users WHERE id=$1`, userID).Scan(&has)
	return has, err
}

func (s *Service) MFASecret(ctx context.Context, userID uuid.UUID) (string, error) {
	var sec string
	err := s.pool.QueryRow(ctx, `SELECT mfa_secret::text FROM users WHERE id=$1`, userID).Scan(&sec)
	return sec, err
}

func (s *Service) LinkOAuthProvider(ctx context.Context, userID uuid.UUID, provider, subject string) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO oauth_providers (user_id, provider, provider_subject) VALUES ($1,$2,$3)
		 ON CONFLICT (provider, provider_subject) DO NOTHING`, userID, provider, subject)
	return err
}

var (
	ErrEmailTaken       = errors.New("email_taken")
	ErrBadCredentials   = errors.New("invalid_credentials")
	ErrUserSuspended    = errors.New("user_suspended")
	ErrTokenExpired     = errors.New("token_expired")
	ErrSessionRevoked   = errors.New("session_revoked")
)

type User struct {
	ID        uuid.UUID
	Email     string
	DisplayName string
	IsVerified bool
	Status    string
}

func (s *Service) Register(ctx context.Context, email, password, displayName string) (*User, string, error) {
	hash, err := HashPassword(password)
	if err != nil { return nil, "", err }
	var u User
	err = s.pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, display_name) VALUES ($1,$2,$3)
		 RETURNING id, email, display_name, is_verified, status`,
		email, hash, displayName).Scan(&u.ID, &u.Email, &u.DisplayName, &u.IsVerified, &u.Status)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, "", ErrEmailTaken
		}
		return nil, "", err   // real DB error → 500, never fake email_taken
	}
	verifyToken, _ := SignActionToken(s.jwtSecret, u.ID.String(), "verify_email", 24*time.Hour)
	return &u, verifyToken, nil
}

func (s *Service) Login(ctx context.Context, email, password, deviceLabel, ip, ua string) (token string, sessionID uuid.UUID, err error) {
	var u User
	var hash string
	err = s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, is_verified, status, password_hash FROM users WHERE email=$1`,
		email).Scan(&u.ID, &u.Email, &u.DisplayName, &u.IsVerified, &u.Status, &hash)
	if err == pgx.ErrNoRows { return "", uuid.Nil, ErrBadCredentials }
	if err != nil { return "", uuid.Nil, err }   // real DB error → 500
	if u.Status != "active" { return "", uuid.Nil, ErrUserSuspended }
	if hash == "" || !CheckPassword(hash, password) { return "", uuid.Nil, ErrBadCredentials }

	jti := uuid.New()
	expires := time.Now().Add(s.tokenTTL)
	_, err = s.pool.Exec(ctx,
		`INSERT INTO user_sessions (user_id, jti, device_label, ip, user_agent, expires_at)
		 VALUES ($1,$2,$3,$4::inet,$5,$6)`, u.ID, jti, deviceLabel, nullIf(ip), ua, expires)
	if err != nil { return "", uuid.Nil, err }
	token, err = IssueToken(s.jwtSecret, u.ID, jti, s.tokenTTL)
	if err != nil { return "", uuid.Nil, err }
	var sessID uuid.UUID
	s.pool.QueryRow(ctx, `SELECT id FROM user_sessions WHERE jti=$1`, jti).Scan(&sessID)
	return token, sessID, nil
}

func (s *Service) Logout(ctx context.Context, jti uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `UPDATE user_sessions SET revoked_at=now() WHERE jti=$1 AND revoked_at IS NULL`, jti)
	return err
}

func (s *Service) ResolveSession(ctx context.Context, jti uuid.UUID) (uuid.UUID, error) {
	var userID uuid.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT user_id FROM user_sessions WHERE jti=$1 AND revoked_at IS NULL AND expires_at > now()`,
		jti).Scan(&userID)
	if err == pgx.ErrNoRows { return uuid.Nil, ErrSessionRevoked }
	return userID, err
}

func (s *Service) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	var u User
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, display_name, is_verified, status FROM users WHERE id=$1`, id).
		Scan(&u.ID, &u.Email, &u.DisplayName, &u.IsVerified, &u.Status)
	if err != nil { return nil, errors.New("user_not_found") }
	return &u, nil
}

func (s *Service) VerifyEmail(ctx context.Context, token string) error {
	jti, userID, err := VerifyActionToken(s.jwtSecret, token, "verify_email")
	if err != nil { return err }
	_ = jti // single-use marker handled by Valkey in handler layer
	_, err = s.pool.Exec(ctx, `UPDATE users SET is_verified=true WHERE id=$1`, userID)
	return err
}

func (s *Service) ListSessions(ctx context.Context, userID uuid.UUID) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, device_label, ip::text, user_agent, expires_at, created_at
		 FROM user_sessions WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC`, userID)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var id uuid.UUID
		var device, ip, ua string
		var exp, created time.Time
		rows.Scan(&id, &device, &ip, &ua, &exp, &created)
		out = append(out, map[string]any{"id":id,"device_label":device,"ip":ip,"user_agent":ua,"expires_at":exp,"created_at":created})
	}
	return out, nil
}

func (s *Service) RevokeSession(ctx context.Context, userID, sessionID uuid.UUID) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE user_sessions SET revoked_at=now() WHERE id=$1 AND user_id=$2`, sessionID, userID)
	return err
}

func nullIf(s string) any { if s == "" { return nil }; return s }
