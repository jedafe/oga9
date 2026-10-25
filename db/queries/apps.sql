-- Ref: oga9-engineering/sections/06-app-framework-registry.md

-- name: ListApps :many
SELECT app_id, version, label, manifest, mvp_required FROM app_registry ORDER BY label;

-- name: GetApp :one
SELECT manifest, mvp_required FROM app_registry WHERE app_id = $1;

-- name: ListInstallations :many
SELECT app_id, status, activated_at, deactivated_at FROM app_installations
WHERE tenant_id = sqlc.arg(tenant_id)::uuid;

-- name: ListActiveApps :many
SELECT app_id FROM app_installations
WHERE tenant_id = sqlc.arg(tenant_id)::uuid AND status = 'active';

-- name: IsAppActive :one
SELECT EXISTS(
  SELECT 1 FROM app_installations
  WHERE tenant_id = sqlc.arg(tenant_id)::uuid AND app_id = sqlc.arg(app_id)
    AND status = 'active'
) AS active;

-- name: ActivateApp :exec
-- Re-activation is an upsert: clears the deactivated marker (idempotent —
-- activating an active app is a no-op state-wise).
INSERT INTO app_installations (tenant_id, app_id, status)
VALUES (sqlc.arg(tenant_id)::uuid, sqlc.arg(app_id), 'active')
ON CONFLICT (tenant_id, app_id) DO UPDATE
SET status = 'active', deactivated_at = NULL, activated_at = now();

-- name: DeactivateApp :exec
UPDATE app_installations SET status = 'deactivated', deactivated_at = now()
WHERE tenant_id = sqlc.arg(tenant_id)::uuid AND app_id = sqlc.arg(app_id);
