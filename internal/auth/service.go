// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
package auth

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Service struct {
	pool     *pgxpool.Pool
	jwtSecret string
	tokenTTL time.Duration
}

func NewService(pool *pgxpool.Pool, jwtSecret string, tokenTTL time.Duration) *Service {
	return &Service{pool: pool, jwtSecret: jwtSecret, tokenTTL: tokenTTL}
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
		return nil, "", ErrEmailTaken
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
	if err != nil { return "", uuid.Nil, ErrBadCredentials }
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
