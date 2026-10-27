-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md (addon model)
-- Add-on catalog + per-tenant purchases. Grants merge into entitlement
-- resolution (plan ∪ addons ∪ license); Rule 25.

CREATE TABLE addon_catalog (
  addon_slug text PRIMARY KEY,
  label text NOT NULL,
  description text,
  app_id text REFERENCES app_registry(app_id),  -- app it unlocks (nullable for cap boosts)
  grants jsonb NOT NULL DEFAULT '{}',           -- {capabilities:[], limit_overrides:{}, limit_deltas:{}}
  price_monthly_minor bigint,
  price_annual_minor bigint,
  currency text NOT NULL DEFAULT 'USD',
  available_on_plans text[] NOT NULL DEFAULT '{}', -- plan slugs that may buy
  active boolean NOT NULL DEFAULT true
);

CREATE TABLE tenant_addons (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  addon_slug text NOT NULL REFERENCES addon_catalog(addon_slug),
  state text NOT NULL DEFAULT 'active',         -- trialing|active|cancelled|expired
  billing_term text NOT NULL DEFAULT 'monthly',
  started_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz,
  cancelled_at timestamptz,
  UNIQUE(tenant_id, addon_slug)
);
CREATE INDEX tenant_addons_tenant ON tenant_addons (tenant_id) WHERE state IN ('trialing','active');

-- seeded catalog (plans-entitlements-catalog.md, indicative pricing)
INSERT INTO addon_catalog (addon_slug, label, description, app_id, grants, price_monthly_minor, price_annual_minor, available_on_plans) VALUES
('elections','Elections','Nominations, secret ballots, ratification','elections',
 '{"capabilities":["app.elections"],"limit_overrides":{}}',900,9000,'{starter,basic}'),
('ai-assistant','AI Assistant','Org-scoped AI chat + HITL drafts','ai-assistant',
 '{"capabilities":["app.ai_assistant"],"limit_overrides":{"limit.ai.requests":500}}',1900,19000,'{basic,professional,business,enterprise}'),
('welfare-pro','Welfare Pro','Advanced welfare case flows','welfare',
 '{"capabilities":["app.welfare"],"limit_overrides":{}}',700,7000,'{basic,professional}'),
('multi-branch','Multi-Branch','+5 branch/structure limit',NULL,
 '{"capabilities":[],"limit_deltas":{"limit.branches":5}}',1500,15000,'{basic,professional}'),
('analytics-pro','Analytics Pro','Consolidation + advanced reports','analytics',
 '{"capabilities":["capability.consolidation"],"limit_overrides":{}}',1200,12000,'{professional}');
