-- Ref: oga9-engineering/sections/06-app-framework-registry.md
CREATE TABLE app_installations (               -- per-tenant activation
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  app_id text NOT NULL REFERENCES app_registry(app_id),
  status text NOT NULL DEFAULT 'active',       -- active|deactivated
  config jsonb NOT NULL DEFAULT '{}',
  activated_at timestamptz NOT NULL DEFAULT now(),
  deactivated_at timestamptz,
  UNIQUE(tenant_id, app_id)
);
