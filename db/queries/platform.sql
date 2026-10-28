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

-- name: CheckpointProvision :exec
-- Crash-safety checkpoint: record tenant/user ids the moment the first
-- provision tx commits, so a stale-lease reclaim RESUMES instead of
-- re-running the fresh tx into a slug conflict + orphan tenant.
UPDATE tenant_provisions SET tenant_id = $2, owner_user_id = $3,
  processing_started_at = now()
WHERE request_id = $1;

-- name: CheckpointProvisionOrg :exec
-- Second crash-safety checkpoint: the org bootstrap commits OUTSIDE the
-- provision tx, so record its id immediately — a crash before
-- FinishProvision must let a reclaim SKIP re-running CreateOrg (a re-run
-- dies on the slug conflict and can never complete).
UPDATE tenant_provisions SET organization_id = $2, processing_started_at = now()
WHERE request_id = $1;

-- name: GetOrgIDByTenantSlug :one
-- Reclaim adoption: organizations.slug is UNIQUE(tenant_id, slug), so a
-- row found here can only be the org THIS provision's crashed run
-- committed before it could checkpoint — never a foreign org.
SELECT id FROM organizations WHERE tenant_id = $1 AND slug = $2;

-- name: StaleProvisions :many
-- Rows whose worker died mid-pipeline: 'processing' past the claim lease.
-- The stored payload IS the retry unit — the redrive sweep re-enqueues
-- tenant:provision and the claim path resumes from the checkpoints.
SELECT * FROM tenant_provisions
WHERE status = 'processing'
  AND COALESCE(processing_started_at, created_at) < now() - interval '30 minutes'
ORDER BY created_at LIMIT 100;

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
-- Operator override wins outright — a stale scheduled_plan_slug queued by
-- a self-serve downgrade must not fire later on top of this write.
UPDATE subscriptions SET plan_slug = $3, state = $4,
  grace_ends_at = COALESCE($5, grace_ends_at), scheduled_plan_slug = NULL
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

-- name: LiveSupportSessionsForOrg :many
-- §15: org admins see live support sessions impersonating ANY member of
-- their org — the banner isn't only visible to the impersonated user.
SELECT DISTINCT ss.id, ss.tenant_id, ss.reason, ss.expires_at, ss.created_at,
       op.display_name AS operator_name, tu.display_name AS target_name
FROM support_sessions ss
JOIN users op ON op.id = ss.operator_user_id
JOIN users tu ON tu.id = ss.target_user_id
JOIN organization_memberships m ON m.user_id = ss.target_user_id
WHERE m.organization_id = sqlc.arg(org_id)::uuid
  AND ss.expires_at > now() AND ss.ended_at IS NULL;

-- name: ClaimProvision :one
-- atomic retry claim — 'failed' rows reclaim; 'processing' rows whose
-- lease is older than 30min reclaim too. processing_started_at re-stamps
-- at every pipeline checkpoint so staleness measures from the last stage
-- commit, not row creation (a live worker mid-run is never reclaimed).
UPDATE tenant_provisions SET status='processing', completed_at=NULL, error=NULL,
  processing_started_at=now()
WHERE request_id=$1 AND (status='failed'
  OR (status='processing'
      AND COALESCE(processing_started_at, created_at) < now() - interval '30 minutes'))
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

-- ---------- tenant export (Rule 28 — §13.06) ----------

-- name: ExportTenantOrgIDs :many
SELECT id FROM organizations WHERE tenant_id = $1;

-- name: ExportScopedTables :many
-- every table carrying a scope column — the export stays complete as the
-- schema grows; no per-table code to forget. BASE TABLE only: views are
-- derived projections, not exportable data (import skips them anyway).
SELECT DISTINCT c.table_name::text FROM information_schema.columns c
JOIN information_schema.tables t ON t.table_name = c.table_name AND t.table_schema = c.table_schema
WHERE c.table_schema = 'public' AND c.column_name = $1 AND t.table_type = 'BASE TABLE'
ORDER BY c.table_name::text;

-- name: ExportTenantUsers :one
-- minimal user projection for the export — never credentials or secrets
-- (ADR-020 data minimization).
SELECT COALESCE(jsonb_agg(j), '[]'::jsonb)::text FROM (
  SELECT jsonb_build_object('id',u.id,'email',u.email,'display_name',u.display_name,
    'status',u.status,'is_verified',u.is_verified,'created_at',u.created_at) AS j
  FROM users u WHERE u.id IN (
    SELECT m.user_id FROM organization_memberships m
    JOIN organizations o ON o.id = m.organization_id WHERE o.tenant_id = $1)
) t;

-- name: ExportTenantRow :one
-- the tenant row itself + its non-secret control-plane rows come through
-- the tenant-scoped dump; this header anchors the export document.
SELECT to_jsonb(t)::text FROM tenants t WHERE t.id = $1;

-- name: ExportChildFKs :many
-- scoped parents' children: rows keyed by FK to a scoped table without
-- carrying the scope column themselves (chat_messages.channel_id,
-- minutes.activity_id, user_role_assignments.role_id, ...). Run once per
-- scope column ('organization_id', 'tenant_id') — without these passes
-- the Rule-28 export silently drops unscoped join/child tables.
SELECT DISTINCT
  kcu.table_name::text  AS child_table,
  kcu.column_name::text AS child_col,
  ccu.table_name::text  AS parent_table,
  ccu.column_name::text AS parent_col
FROM information_schema.table_constraints tc
JOIN information_schema.key_column_usage kcu
  ON tc.constraint_name = kcu.constraint_name AND tc.table_schema = kcu.table_schema
JOIN information_schema.constraint_column_usage ccu
  ON tc.constraint_name = ccu.constraint_name AND tc.table_schema = ccu.table_schema
WHERE tc.constraint_type = 'FOREIGN KEY' AND tc.table_schema = 'public'
  AND ccu.table_name IN (
    SELECT table_name FROM information_schema.columns
    WHERE table_schema = 'public' AND column_name = sqlc.arg(scope_col)::text)
ORDER BY 1;

-- name: ExportTenantUserNotifications :one
-- Notifications with NULL organization_id (platform/system-scoped rows)
-- still belong to tenant users — the scoped dump keys on the column so
-- they'd be missed without this pass.
SELECT COALESCE(jsonb_agg(t),'[]'::jsonb)::text FROM notifications t
WHERE t.organization_id IS NULL AND t.user_id IN (
  SELECT m.user_id FROM organization_memberships m
  WHERE m.organization_id = ANY(sqlc.arg(org_ids)::uuid[]));

-- name: ExportOrgNotificationPrefs :one
-- preferences are user-keyed (no scope column) — join through the org's
-- memberships so member prefs land in the export too.
SELECT COALESCE(jsonb_agg(t),'[]'::jsonb)::text FROM notification_preferences t
WHERE t.user_id IN (
  SELECT m.user_id FROM organization_memberships m
  WHERE m.organization_id = ANY(sqlc.arg(org_ids)::uuid[]));

-- name: CreateTenantExport :one
INSERT INTO tenant_exports (tenant_id, requested_by, options) VALUES ($1,$2,$3) RETURNING *;

-- name: GetTenantExport :one
SELECT * FROM tenant_exports WHERE id = $1 AND tenant_id = $2;

-- name: MarkTenantExportDone :execrows
-- CAS: only a live/failed row finishes — a retry after a marked failure
-- CAN complete (the blob is valid; 'failed' just records the last error).
UPDATE tenant_exports SET status='done', storage_key=$2, bundle_version=$3, error=NULL, completed_at=now(), options=options-'passphrase_enc'
WHERE id=$1 AND status IN ('processing','failed');

-- name: MarkTenantExportFailed :exec
-- passphrase_enc stays: asynq retries need to decrypt it again — a
-- transient failure must not turn into a permanent one. Only
-- MarkTenantExportDone scrubs the sealed secret.
UPDATE tenant_exports SET status='failed', error=$2, completed_at=now()
WHERE id=$1 AND status='processing';

-- name: GetTenantExportByID :one
SELECT * FROM tenant_exports WHERE id = $1;

-- name: ConfirmTenantExport :execrows
-- CAS: confirm lands once, only on a finished bundle.
UPDATE tenant_exports SET confirmed_at=now()
WHERE id=$1 AND tenant_id=$2 AND status='done' AND confirmed_at IS NULL;

-- name: ExportTenantUsersCredentialed :one
-- ADR-026 migration export ONLY: password_hash rides inside an encrypted,
-- explicitly-requested bundle — never the default Rule-28 path.
SELECT COALESCE(jsonb_agg(to_jsonb(u)), '[]'::jsonb)::text
FROM users u WHERE u.id IN (
  SELECT m.user_id FROM organization_memberships m
  JOIN organizations o ON o.id = m.organization_id WHERE o.tenant_id = $1);

-- name: SoftDeleteTenantOrgs :exec
-- ADR-026 post_state=purge: orgs soft-delete; platform:data_erasure does
-- the eventual hard delete (Rule 11 — the only hard-delete path).
UPDATE organizations SET deleted_at=now() WHERE tenant_id=$1 AND deleted_at IS NULL;

-- name: ExportInboundExternalEdges :one
-- ADR-026: edges where one of our orgs is the relationship TARGET but the
-- source org lives outside this tenant — unimportable (source absent);
-- recorded as dropped_external in the bundle manifest.
SELECT COALESCE(jsonb_agg(to_jsonb(t)),'[]'::jsonb)::text
FROM organization_relationships t
WHERE t.related_org_id = ANY(sqlc.arg(org_ids)::uuid[])
  AND NOT (t.organization_id = ANY(sqlc.arg(org_ids)::uuid[]));

-- name: GetFeatureFlag :one
SELECT enabled FROM feature_flags WHERE key = $1;
