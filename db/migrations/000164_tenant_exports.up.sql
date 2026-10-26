-- Ref: oga9-engineering/sections/15-subscription-admin.md + Rule 28 —
-- durable export rows for the async tenant:export worker (mirrors
-- member_exports; blob storage holds the JSON, status tracks the job).
CREATE TABLE tenant_exports (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  requested_by uuid NOT NULL REFERENCES users(id),
  storage_key text,
  error text,
  status text NOT NULL DEFAULT 'processing',  -- processing|done|failed
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz
);
CREATE INDEX tenant_exports_tenant ON tenant_exports (tenant_id, created_at DESC);
