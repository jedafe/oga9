-- Ref: oga9-engineering/sections/15-subscription-admin.md — control-plane queries.
-- Platform scope is tenant-scoped (Rule 24): operators never pass tenant_id
-- from the request body alone; the catalog requires a platform.* node.

-- ---------- tenant_provisions ----------
-- name: CreateProvision :one
-- payload is the serialized request — replay compares it to detect
-- request_id reuse with a different intent (idempotency mismatch).
INSERT INTO tenant_provisions (request_id, payload)
VALUES (sqlc.arg(request_id), sqlc.arg(payload)::jsonb)
ON CONFLICT (request_id) DO NOTHING RETURNING *;

-- name: GetProvision :one
SELECT * FROM tenant_provisions WHERE request_id = $1;

-- name: FinishProvision :execrows
UPDATE tenant_provisions SET status = $2, tenant_id = $3,
  organization_id = $4, owner_user_id = $5, error = $6, completed_at = now()
WHERE request_id = $1;

-- ---------- tenants ----------
-- name: ListTenants :many
-- LATERAL pins the newest live subscription — a tenant with multiple
-- non-cancelled subs must not double-row the listing.
SELECT t.*, s.plan_slug, s.state AS subscription_state,
       (SELECT count(*) FROM organizations o WHERE o.tenant_id = t.id AND o.deleted_at IS NULL) AS org_count
FROM tenants t
LEFT JOIN LATERAL (
  SELECT sub.plan_slug, sub.state FROM subscriptions sub
  WHERE sub.tenant_id = t.id AND sub.state <> 'cancelled'
  ORDER BY sub.created_at DESC LIMIT 1
) s ON true
WHERE ($1::text = '' OR t.status = $1)
ORDER BY t.created_at DESC LIMIT 200;

-- name: GetTenantRow :one
SELECT * FROM tenants WHERE id = $1;

-- name: CreateTenantRow :one
INSERT INTO tenants (name, slug, billing_email, edition, status)
VALUES (sqlc.arg(name), sqlc.arg(slug), sqlc.arg(billing_email),
        sqlc.arg(edition), 'trial')
RETURNING *;

-- name: SetTenantStatus :execrows
UPDATE tenants SET status = $2 WHERE id = $1;

-- ---------- subscriptions / plans ----------
-- name: ListSubscriptions :many
SELECT s.*, t.name AS tenant_name, t.slug AS tenant_slug FROM subscriptions s
JOIN tenants t ON t.id = s.tenant_id
WHERE ($1::text = '' OR s.state = $1)
ORDER BY s.created_at DESC LIMIT 200;

-- name: OverrideSubscription :execrows
UPDATE subscriptions SET plan_slug = $3, state = $4,
  grace_ends_at = COALESCE($5, grace_ends_at)
WHERE id = $1 AND tenant_id = $2;

-- name: UpsertPlan :one
INSERT INTO plans (slug, label, price_monthly_minor, price_annual_minor,
  currency, is_public, sort)
VALUES (sqlc.arg(slug), sqlc.arg(label), sqlc.arg(price_monthly_minor),
  sqlc.arg(price_annual_minor), sqlc.arg(currency), sqlc.arg(is_public),
  sqlc.arg(sort))
ON CONFLICT (slug) DO UPDATE SET label = EXCLUDED.label,
  price_monthly_minor = EXCLUDED.price_monthly_minor,
  price_annual_minor = EXCLUDED.price_annual_minor,
  currency = EXCLUDED.currency, is_public = EXCLUDED.is_public,
  sort = EXCLUDED.sort
RETURNING *;

-- ---------- deployments ----------
-- name: ListDeployments :many
SELECT d.*, t.name AS tenant_name FROM deployments d
JOIN tenants t ON t.id = d.tenant_id
WHERE (sqlc.arg(tenant_id)::uuid IS NULL OR d.tenant_id = sqlc.arg(tenant_id)::uuid)
ORDER BY d.created_at DESC LIMIT 200;

-- name: CreateDeployment :one
INSERT INTO deployments (tenant_id, kind, edition, deployment_key)
VALUES (sqlc.arg(tenant_id), sqlc.arg(kind), sqlc.arg(edition),
        sqlc.arg(deployment_key))
RETURNING *;

-- ---------- support sessions ----------
-- name: CreateSupportSession :one
INSERT INTO support_sessions (operator_user_id, target_user_id, tenant_id, reason, expires_at)
VALUES (sqlc.arg(operator_user_id), sqlc.arg(target_user_id),
        sqlc.arg(tenant_id), sqlc.arg(reason), sqlc.arg(expires_at))
RETURNING *;

-- name: GetSupportSession :one
SELECT * FROM support_sessions WHERE id = $1
  AND expires_at > now() AND ended_at IS NULL;

-- name: EndSupportSession :execrows
UPDATE support_sessions SET ended_at = now() WHERE id = $1 AND ended_at IS NULL;

-- ---------- feature flags ----------
-- name: ListFeatureFlags :many
SELECT * FROM feature_flags ORDER BY key;

-- name: UpsertFeatureFlag :exec
INSERT INTO feature_flags (key, enabled, rollout, updated_at)
VALUES (sqlc.arg(key), sqlc.arg(enabled), COALESCE(sqlc.narg(rollout)::jsonb,'{}'), now())
ON CONFLICT (key) DO UPDATE SET enabled = EXCLUDED.enabled,
  rollout = EXCLUDED.rollout, updated_at = now();

-- ---------- audit + usage ----------
-- name: ListAuditEvents :many
SELECT * FROM audit_events
WHERE (sqlc.arg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.arg(tenant_id)::uuid)
  AND (sqlc.arg(action)::text = '' OR action = sqlc.arg(action))
ORDER BY created_at DESC LIMIT 200;

-- name: ListUsage :many
SELECT * FROM usage_meters
WHERE (sqlc.arg(tenant_id)::uuid IS NULL OR tenant_id = sqlc.arg(tenant_id)::uuid)
ORDER BY period_start DESC, limit_key;

-- name: ProvisionUser :one
-- Owner user created by the provision pipeline — no password (welcome
-- action-token flow); verified=false until they set credentials.
INSERT INTO users (id, email, display_name, status)
VALUES (gen_random_uuid(), sqlc.arg(email), sqlc.arg(display_name), 'active')
ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
RETURNING *;
;
-- name: LiveSupportSessionsForUser :many
-- §15 banner: the impersonated user's client surfaces the active support
-- session AND the operator identity — user-scoped read, live rows only.
SELECT ss.id, ss.tenant_id, ss.reason, ss.expires_at, ss.created_at,
       op.display_name AS operator_name
FROM support_sessions ss
JOIN users op ON op.id = ss.operator_user_id
WHERE ss.target_user_id = $1 AND ss.expires_at > now() AND ss.ended_at IS NULL;

-- name: ClaimProvision :one
-- atomic retry claim — 'failed' rows reclaim; 'processing' rows whose
-- lease is older than 10min reclaim too. processing_started_at stamps
-- each claim so staleness measures from the last claim, not row creation
-- (a live worker past the window is never reclaimed mid-run).
UPDATE tenant_provisions SET status='processing', completed_at=NULL, error=NULL,
  processing_started_at=now()
WHERE request_id=$1 AND (status='failed'
  OR (status='processing'
      AND COALESCE(processing_started_at, created_at) < now() - interval '10 minutes'))
RETURNING *;

-- name: UserInTenant :one
-- §15 impersonation scope: the target must actually belong to the tenant
-- the operator is scoped to — the request's tenant_id is advisory only
-- without a membership join proving it (Rule 24).
SELECT EXISTS (
  SELECT 1 FROM organization_memberships m
  JOIN organizations o ON o.id = m.organization_id
  WHERE m.user_id = $1 AND o.tenant_id = $2
    AND m.lifecycle_state NOT IN ('resigned','expelled','deceased','withdrawn','rejected'))
;

-- name: RevokeSessionsForSupport :execrows
-- end impersonation → the operator's minted session dies with the
-- support_sessions row (explicit end must cut access, not just flag it).
UPDATE user_sessions SET revoked_at = now()
WHERE support_session_id = $1 AND revoked_at IS NULL;

-- name: UserHasAnyPlatformRole :one
-- platform operator bypass (login + org-ctx suspend gates): ANY
-- platform.* node on a NULL-tenant role counts — the check must not
-- require a specific node like tenants.manage.
SELECT EXISTS (
  SELECT 1 FROM user_role_assignments ura
  JOIN user_roles r ON r.id = ura.role_id
  JOIN role_permissions rp ON rp.role_id = r.id
  WHERE ura.user_id = $1 AND r.tenant_id IS NULL
    AND rp.permission LIKE 'platform.%')
;
