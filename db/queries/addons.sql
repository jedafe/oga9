-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md — add-ons

-- name: ListAddonCatalog :many
SELECT * FROM addon_catalog ORDER BY addon_slug;

-- name: ListAddonCatalogActive :many
SELECT * FROM addon_catalog WHERE active ORDER BY addon_slug;

-- name: GetAddon :one
SELECT * FROM addon_catalog WHERE addon_slug = $1;

-- name: UpsertAddon :one
INSERT INTO addon_catalog (addon_slug, label, description, app_id, grants,
  price_monthly_minor, price_annual_minor, currency, available_on_plans, active)
VALUES (sqlc.arg(addon_slug), sqlc.arg(label), sqlc.narg(description),
  sqlc.narg(app_id), sqlc.arg(grants), sqlc.narg(price_monthly_minor),
  sqlc.narg(price_annual_minor), sqlc.arg(currency),
  sqlc.arg(available_on_plans), sqlc.arg(active))
ON CONFLICT (addon_slug) DO UPDATE SET
  label = EXCLUDED.label, description = EXCLUDED.description,
  app_id = EXCLUDED.app_id, grants = EXCLUDED.grants,
  price_monthly_minor = EXCLUDED.price_monthly_minor,
  price_annual_minor = EXCLUDED.price_annual_minor,
  currency = EXCLUDED.currency,
  available_on_plans = EXCLUDED.available_on_plans,
  active = EXCLUDED.active
RETURNING *;

-- name: ListTenantAddons :many
SELECT ta.*, ac.label FROM tenant_addons ta
JOIN addon_catalog ac ON ac.addon_slug = ta.addon_slug
WHERE ta.tenant_id = $1 ORDER BY ta.started_at DESC;

-- name: GetTenantAddon :one
SELECT * FROM tenant_addons WHERE tenant_id = $1 AND addon_slug = $2;

-- name: SubscribeTenantAddon :one
-- (re)activate: a cancelled/expired row re-subscribes in place.
INSERT INTO tenant_addons (tenant_id, addon_slug, state, billing_term, expires_at)
VALUES (sqlc.arg(tenant_id)::uuid, sqlc.arg(addon_slug), 'active',
        sqlc.arg(billing_term), sqlc.narg(expires_at)::timestamptz)
ON CONFLICT (tenant_id, addon_slug) DO UPDATE SET
  state = 'active', billing_term = EXCLUDED.billing_term,
  expires_at = EXCLUDED.expires_at, cancelled_at = NULL, started_at = now()
RETURNING *;

-- name: CancelTenantAddon :execrows
UPDATE tenant_addons SET state = 'cancelled', cancelled_at = now()
WHERE tenant_id = $1 AND addon_slug = $2 AND state IN ('trialing','active');

-- name: ExpireTenantAddons :many
-- sweep: past-expiry active/trialing addons lapse (emits AddonExpired per row).
UPDATE tenant_addons SET state = 'expired'
WHERE state IN ('trialing','active') AND expires_at IS NOT NULL AND expires_at < now()
RETURNING tenant_id, addon_slug;

-- name: ListActiveAddonGrants :many
-- entitlement merge source (§07 resolution order): live grants only.
SELECT ac.grants FROM tenant_addons ta
JOIN addon_catalog ac ON ac.addon_slug = ta.addon_slug
WHERE ta.tenant_id = $1
  AND ta.state IN ('trialing','active')
  AND (ta.expires_at IS NULL OR ta.expires_at > now())
  AND ac.active;
