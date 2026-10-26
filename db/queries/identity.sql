-- Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
-- sqlc queries for users, sessions, oauth links (Phase 02).

-- name: CreateUser :one
INSERT INTO users (email, password_hash, display_name)
VALUES (sqlc.arg(email), sqlc.arg(password_hash), sqlc.arg(display_name))
RETURNING id, email, display_name, is_verified, status;

-- name: CreateOAuthUser :one
INSERT INTO users (email, display_name, is_verified, status)
VALUES (sqlc.arg(email), sqlc.arg(display_name), sqlc.arg(is_verified), 'active')
RETURNING id;

-- name: GetUserByEmail :one
SELECT id, email, display_name, is_verified, status, password_hash, mfa_secret
FROM users WHERE email = sqlc.arg(email);

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
SELECT mfa_secret FROM users WHERE id = $1;

-- name: CreateSession :one
INSERT INTO user_sessions (user_id, jti, device_label, ip, user_agent, expires_at, support_session_id)
VALUES (sqlc.arg(user_id), sqlc.arg(jti), sqlc.arg(device_label),
        NULLIF(sqlc.arg(ip)::text, '')::inet, sqlc.arg(user_agent), sqlc.arg(expires_at),
        sqlc.narg(support_session_id)::uuid)
RETURNING id;

-- name: GetSessionByJTI :one
SELECT s.id, s.user_id, s.revoked_at, s.expires_at, s.support_session_id, u.status AS user_status
FROM user_sessions s
JOIN users u ON u.id = s.user_id
WHERE s.jti = $1;

-- name: ListUserSessions :many
SELECT id, jti, device_label, ip::text AS ip, user_agent, created_at, expires_at, support_session_id
FROM user_sessions
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY created_at DESC;

-- name: RevokeSessionByJTI :one
UPDATE user_sessions SET revoked_at = now() WHERE jti = $1 AND revoked_at IS NULL
RETURNING id;

-- name: RevokeSessionByID :one
UPDATE user_sessions SET revoked_at = now()
WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL
RETURNING jti;

-- name: RevokeAllUserSessions :many
UPDATE user_sessions SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL
RETURNING jti;

-- name: LinkOAuthProvider :exec
INSERT INTO oauth_providers (user_id, provider, provider_subject)
VALUES ($1, $2, $3)
ON CONFLICT (provider, provider_subject) DO NOTHING;

-- name: GetUserByOAuthSubject :one
SELECT user_id FROM oauth_providers WHERE provider = $1 AND provider_subject = $2;

-- name: SetJapaconnectUserID :exec
UPDATE users SET japaconnect_user_id = sqlc.arg(japaconnect_user_id) WHERE id = sqlc.arg(user_id);

-- name: GetRecoveryCodes :one
SELECT mfa_recovery_codes FROM users WHERE id = $1;

-- name: SetRecoveryCodes :exec
UPDATE users SET mfa_recovery_codes = $2 WHERE id = $1;

-- name: ConsumeRecoveryCode :one
-- Atomic single-use burn: removes the hash and returns the row in one
-- statement — concurrent logins can't both consume the same code.
UPDATE users SET mfa_recovery_codes =
  COALESCE(
    (SELECT jsonb_agg(e) FROM jsonb_array_elements_text(mfa_recovery_codes) e
     WHERE e <> sqlc.arg(hash)::text),
    '[]'::jsonb)
WHERE id = sqlc.arg(id)::uuid
  AND EXISTS (SELECT 1 FROM jsonb_array_elements_text(mfa_recovery_codes) x
              WHERE x = sqlc.arg(hash)::text)
RETURNING id;

-- name: GetUserAuthState :one
SELECT id, password_hash, mfa_secret, status, is_verified
FROM users WHERE id = $1;

-- name: UserRequiresMFA :one
SELECT EXISTS (
  SELECT 1
  FROM user_role_assignments ura
  JOIN user_roles r ON r.id = ura.role_id
  LEFT JOIN role_permissions rp ON rp.role_id = r.id
  LEFT JOIN permissions p ON p.node = rp.permission
  WHERE ura.user_id = sqlc.arg(user_id)
    AND (r.slug = 'org_admin' OR p.node LIKE 'platform.%')
) AS requires_mfa;

-- name: UserHasHealthyMembership :one
-- §15 partial access: a user with a live membership in ANY non-suspended
-- tenant keeps login; only fully-suspended users are blocked.
SELECT EXISTS (
  SELECT 1 FROM organization_memberships m
  JOIN organizations o ON o.id = m.organization_id
  JOIN tenants t ON t.id = o.tenant_id
  WHERE m.user_id = $1 AND t.status <> 'suspended'
    AND m.lifecycle_state NOT IN ('resigned','expelled','deceased'));
