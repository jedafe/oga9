-- Ref: oga9-engineering/sections/06-app-framework-registry.md
-- Backfill: tenants provisioned before the registry landed have no
-- installations — RequireAppActive would 403 every module route for them.
-- MVP-required apps activate by default (provisionMVPApps parity).
INSERT INTO app_installations (tenant_id, app_id, status)
SELECT t.id, a.app_id, 'active'
FROM tenants t CROSS JOIN app_registry a
WHERE a.mvp_required
ON CONFLICT (tenant_id, app_id) DO NOTHING;
