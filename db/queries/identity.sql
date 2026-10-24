-- Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
-- sqlc queries for users, sessions, oauth links (Phase 02).

-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name)
VALUES ($1, $2, $3)
RETURNING id, email, display_name, is_verified, status;

-- name: CreateOAuthUser :one
INSERT INTO users (email, display_name, is_verified, status)
VALUES ($1, $2, $3, 'active')
RETURNING id;

-- name: GetUserByEmail :one
SELECT id, email, display_name, is_verified, status, password_hash,
       COALESCE(convert_from(mfa_secret, 'UTF8'), '')::text AS mfa_secret
FROM users WHERE email = $1;

-- name: GetUserByID :one
SELECT id, email, display_name, is_verified, status
FROM users WHERE id = $1;

-- name: GetUserIDByEmail :one
SELECT id FROM users WHERE email = $1;

-- name: SetUserVerified :exec
UPDATE users SET is_verified = true WHERE id = $1;

-- name: UpdateUserPassword :exec
UPDATE users SET password_hash = $1 WHERE id = $2;

-- name: SetMFASecret :exec
UPDATE users SET mfa_secret = $1 WHERE id = $2;

-- name: GetMFASecret :one
SELECT COALESCE(convert_from(mfa_secret, 'UTF8'), '')::text AS mfa_secret FROM users WHERE id = $1;

-- name: CreateSession :one
INSERT INTO user_sessions (user_id, jti, device_label, ip, user_agent, expires_at)
VALUES ($1, $2, $3, $4::inet, $5, $6)
RETURNING id;

-- name: GetSessionByJTI :one
SELECT id, user_id, revoked_at, expires_at FROM user_sessions WHERE jti = $1;

-- name: ListUserSessions :many
SELECT id, device_label, ip::text AS ip, user_agent, created_at, expires_at
FROM user_sessions
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY created_at DESC;

-- name: RevokeSessionByJTI :exec
UPDATE user_sessions SET revoked_at = now() WHERE jti = $1 AND revoked_at IS NULL;

-- name: RevokeSessionByID :exec
UPDATE user_sessions SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: RevokeAllUserSessions :exec
UPDATE user_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL;

-- name: LinkOAuthProvider :exec
INSERT INTO oauth_providers (user_id, provider, provider_subject)
VALUES ($1, $2, $3)
ON CONFLICT (provider, provider_subject) DO NOTHING;

-- name: GetUserByOAuthSubject :one
SELECT user_id FROM oauth_providers WHERE provider = $1 AND provider_subject = $2;

-- name: EmitDomainEvent :exec
INSERT INTO domain_events (name, aggregate_type, aggregate_id, payload)
VALUES ($1, $2, $3, $4);
