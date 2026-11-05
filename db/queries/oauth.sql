-- Ref: oga9-engineering/sections/29-wordpress-connector.md + ADR-028 —
-- sqlc queries for the OIDC/OAuth2 provider slice (oauth_clients,
-- oauth_authorization_codes, oauth_grants).

-- name: CreateOAuthClient :one
INSERT INTO oauth_clients (kind, connector_id, organization_id, client_id, hashed_secret, redirect_uris, name, trusted)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetOAuthClientByClientID :one
SELECT * FROM oauth_clients WHERE client_id = $1;

-- name: GetOAuthClient :one
SELECT * FROM oauth_clients WHERE id = $1;

-- name: ListOAuthClients :many
SELECT * FROM oauth_clients ORDER BY created_at DESC;

-- name: UpdateOAuthClient :exec
UPDATE oauth_clients SET name = $2, redirect_uris = $3, trusted = $4 WHERE id = $1;

-- name: SetOAuthClientStatus :exec
UPDATE oauth_clients SET status = $2, revoked_at = CASE WHEN $2 = 'disabled' THEN now() ELSE NULL END
WHERE id = $1;

-- name: CreateAuthorizationCode :exec
INSERT INTO oauth_authorization_codes (code, client_id, user_id, organization_id, redirect_uri, code_challenge, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, now() + interval '120 seconds');

-- name: ConsumeAuthorizationCode :one
UPDATE oauth_authorization_codes SET consumed_at = now()
WHERE code = $1 AND consumed_at IS NULL
RETURNING *;

-- name: CreateOAuthGrant :one
INSERT INTO oauth_grants (client_id, user_id, organization_id, session_id, refresh_token_hash, refresh_expires_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetOAuthGrantByRefreshHash :one
SELECT * FROM oauth_grants
WHERE client_id = $1 AND refresh_token_hash = $2 AND revoked_at IS NULL;

-- name: GetOAuthGrantBySession :one
SELECT * FROM oauth_grants WHERE session_id = $1 AND revoked_at IS NULL;

-- name: GetOAuthGrantByID :one
SELECT * FROM oauth_grants WHERE id = $1;

-- name: RotateOAuthGrant :execrows
UPDATE oauth_grants SET session_id = $2, refresh_token_hash = $3, refresh_expires_at = $4
WHERE id = $1 AND session_id = $5 AND revoked_at IS NULL;

-- name: RevokeOAuthGrant :exec
UPDATE oauth_grants SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeClientGrants :exec
UPDATE oauth_grants SET revoked_at = now() WHERE client_id = $1 AND revoked_at IS NULL;
