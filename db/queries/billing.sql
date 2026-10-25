-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md

-- name: ListPlans :many
SELECT slug, label, price_monthly_minor, price_annual_minor, currency, sort
FROM plans WHERE is_public ORDER BY sort;

-- name: GetPlan :one
SELECT slug, price_monthly_minor, price_annual_minor, is_public
FROM plans WHERE slug = $1;

-- name: GetPlanCapabilities :many
SELECT capability FROM plan_capabilities WHERE plan_slug = $1;

-- name: GetPlanLimits :many
SELECT limit_key, limit_value FROM plan_limits WHERE plan_slug = $1;

-- name: GetSubscription :one
-- Latest subscription row is the current one.
SELECT id, tenant_id, plan_slug, state, billing_term, trial_ends_at,
       current_period_end, grace_ends_at, cancelled_at, created_at
FROM subscriptions WHERE tenant_id = sqlc.arg(tenant_id)::uuid
ORDER BY created_at DESC LIMIT 1;

-- name: CreateSubscription :one
INSERT INTO subscriptions (tenant_id, plan_slug, state, billing_term, trial_ends_at)
VALUES (sqlc.arg(tenant_id)::uuid, sqlc.arg(plan_slug), sqlc.arg(state),
        sqlc.arg(billing_term), sqlc.arg(trial_ends_at))
RETURNING id;

-- name: SetSubscriptionState :exec
-- Lifecycle machine writes: state + whichever deadline columns the
-- transition touches (NULL args leave columns unchanged).
UPDATE subscriptions SET
  state = sqlc.arg(state),
  grace_ends_at = COALESCE(sqlc.arg(grace_ends_at)::timestamptz, grace_ends_at),
  current_period_end = COALESCE(sqlc.arg(current_period_end)::timestamptz, current_period_end),
  cancelled_at = COALESCE(sqlc.arg(cancelled_at)::timestamptz, cancelled_at)
WHERE id = sqlc.arg(id)::uuid;

-- name: SetSubscriptionPlan :exec
UPDATE subscriptions SET plan_slug = sqlc.arg(plan_slug)
WHERE id = sqlc.arg(id)::uuid;

-- name: TransitionSubscriptionState :execrows
-- Compare-and-set: fires only while the subscription still holds the state
-- the caller read — concurrent lifecycle writers can't regress or
-- double-emit. 0 rows = another transition already moved it.
UPDATE subscriptions SET
  state = sqlc.arg(to_state),
  grace_ends_at = COALESCE(sqlc.arg(grace_ends_at)::timestamptz, grace_ends_at),
  current_period_end = COALESCE(sqlc.arg(current_period_end)::timestamptz, current_period_end),
  cancelled_at = COALESCE(sqlc.arg(cancelled_at)::timestamptz, cancelled_at)
WHERE id = sqlc.arg(id)::uuid AND state = sqlc.arg(from_state);

-- name: ListDueSubscriptions :many
-- Sweep set: trials past expiry, past_due/grace rows past their grace
-- deadline, and active rows whose period lapsed without a provider event
-- (silent provider → lapse to past_due, never free-forever). The state
-- machine moves each one step per pass.
SELECT id, tenant_id, plan_slug, state FROM subscriptions
WHERE (state = 'trial' AND trial_ends_at IS NOT NULL AND trial_ends_at < now())
   OR (state IN ('past_due','grace') AND grace_ends_at IS NOT NULL AND grace_ends_at < now())
   OR (state = 'active' AND current_period_end IS NOT NULL AND current_period_end < now());

-- name: InsertLicense :one
-- Re-issue on the same deployment_id is a renewal: rotate the signed
-- material in place (UNIQUE deployment_id is one-license-per-deployment).
INSERT INTO licenses (tenant_id, deployment_id, edition, entitlements, expires_at, signature)
VALUES (sqlc.arg(tenant_id)::uuid, sqlc.arg(deployment_id), sqlc.arg(edition),
        sqlc.arg(entitlements)::jsonb, sqlc.arg(expires_at), sqlc.arg(signature))
ON CONFLICT (deployment_id) DO UPDATE SET
  tenant_id = EXCLUDED.tenant_id, edition = EXCLUDED.edition,
  entitlements = EXCLUDED.entitlements, expires_at = EXCLUDED.expires_at,
  signature = EXCLUDED.signature, issued_at = now()
RETURNING id;

-- name: GetLicenseByDeployment :one
SELECT id, tenant_id, entitlements, expires_at, signature FROM licenses
WHERE deployment_id = sqlc.arg(deployment_id) ORDER BY issued_at DESC LIMIT 1;

-- name: GetLatestLicense :one
-- Entitlement merge source: unexpired license for the tenant, newest first.
SELECT id, entitlements, expires_at FROM licenses
WHERE tenant_id = sqlc.arg(tenant_id)::uuid AND expires_at > now()
ORDER BY issued_at DESC LIMIT 1;

-- name: InsertWebhookEvent :one
-- Durable raw record + replay guard (UNIQUE provider,event_id) — a replay
-- returns no row so the endpoint can ack and skip.
INSERT INTO billing_webhook_events (provider, event_id, event_type, payload)
VALUES (sqlc.arg(provider), sqlc.arg(event_id), sqlc.arg(event_type), sqlc.arg(payload)::jsonb)
ON CONFLICT (provider, event_id) DO NOTHING
RETURNING id;

-- name: GetWebhookEvent :one
SELECT id, provider, event_id, event_type, payload, processed_at
FROM billing_webhook_events WHERE id = sqlc.arg(id)::uuid;

-- name: GetWebhookEventForUpdate :one
-- Row lock serializes concurrent processors of the same event (Rule 27 —
-- processed_at rechecked under lock inside the tx).
SELECT id, provider, event_id, event_type, payload, processed_at
FROM billing_webhook_events WHERE id = sqlc.arg(id)::uuid FOR UPDATE;

-- name: MarkWebhookProcessed :execrows
-- Idempotent worker completion — a redrive sees processed_at set and skips.
UPDATE billing_webhook_events SET processed_at = now()
WHERE id = sqlc.arg(id)::uuid AND processed_at IS NULL;

-- name: ListStaleWebhookEvents :many
-- Rule 9 durability floor: rows recorded but never processed because the
-- enqueue failed. The sweep re-drives them into billing:process_webhook.
-- Bounded to 2 days — a permanently-poisoned row must stop re-enqueueing
-- (manual inspection), not loop forever.
SELECT id FROM billing_webhook_events
WHERE processed_at IS NULL
  AND created_at < now() - interval '5 minutes'
  AND created_at > now() - interval '2 days'
ORDER BY created_at LIMIT 200;

-- name: GetUsage :one
SELECT used FROM usage_meters
WHERE tenant_id = sqlc.arg(tenant_id)::uuid AND limit_key = sqlc.arg(limit_key)
  AND period_start = sqlc.arg(period_start)::date;

-- name: AddUsage :execrows
INSERT INTO usage_meters (tenant_id, limit_key, used, period_start)
VALUES (sqlc.arg(tenant_id)::uuid, sqlc.arg(limit_key), sqlc.arg(delta)::bigint,
        sqlc.arg(period_start)::date)
ON CONFLICT (tenant_id, limit_key, period_start) DO UPDATE SET used = usage_meters.used + sqlc.arg(delta);

-- name: ListTenantsWithNode :many
-- Billing-tenant resolution (Rule 24): tenants where the caller holds a
-- tenant-scoped role granting the permission node — the api-routes-catalog
-- gate for /billing/* is org.admin.billing, not a role name (Rule 20).
SELECT DISTINCT r.tenant_id FROM user_role_assignments ura
JOIN user_roles r ON r.id = ura.role_id
JOIN role_permissions rp ON rp.role_id = r.id
WHERE ura.user_id = sqlc.arg(user_id)::uuid
  AND r.tenant_id IS NOT NULL AND rp.permission = sqlc.arg(node);

-- name: HasPlatformNode :one
-- platform.* nodes resolve via NULL-tenant roles only (super-admin scope —
-- org role clones never carry platform nodes).
SELECT EXISTS (
  SELECT 1 FROM user_role_assignments ura
  JOIN user_roles r ON r.id = ura.role_id
  JOIN role_permissions rp ON rp.role_id = r.id
  WHERE ura.user_id = sqlc.arg(user_id)::uuid
    AND r.tenant_id IS NULL AND rp.permission = sqlc.arg(node)
) AS allowed;
