-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
CREATE TABLE licenses (                         -- self-hosted, Rule 29
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  deployment_id text NOT NULL UNIQUE,
  edition text NOT NULL DEFAULT 'self_hosted',
  entitlements jsonb NOT NULL,                  -- signed feature set+limits
  issued_at timestamptz NOT NULL DEFAULT now(),
  expires_at timestamptz NOT NULL,
  signature bytea NOT NULL                      -- signed license file
);
