-- Ref: oga9-engineering/sections/15-subscription-admin.md — platform admin
-- (g13 reserved range 000120–129 was never used; per MIGRATION-ORDER the
-- next free number wins over backfilling a skipped range).

-- durable provision record — tenant:provision is idempotent on request_id.
CREATE TABLE tenant_provisions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  request_id text NOT NULL UNIQUE,
  tenant_id uuid REFERENCES tenants(id),
  organization_id uuid REFERENCES organizations(id),
  owner_user_id uuid REFERENCES users(id),
  error text,
  status text NOT NULL DEFAULT 'processing',  -- processing|done|failed
  created_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz
);

-- deployments = installations of OGA9 a tenant may run (saas or self-hosted
-- under a signed license — licenses.deployment_id references these).
CREATE TABLE deployments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  kind text NOT NULL DEFAULT 'saas',         -- saas|self_hosted
  edition text NOT NULL DEFAULT 'community', -- community|pro|enterprise
  deployment_key text NOT NULL UNIQUE,       -- appears on issued licenses
  last_seen_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX deployments_tenant ON deployments (tenant_id);

-- support impersonation sessions (§15: audit-first write, 30min TTL).
-- audit_events row lands BEFORE this insert (Rule 10) — session tokens
-- carry support_session_id so the banner can resolve it.
CREATE TABLE support_sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  operator_user_id uuid NOT NULL REFERENCES users(id),
  target_user_id uuid NOT NULL REFERENCES users(id),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  reason text,
  expires_at timestamptz NOT NULL,
  ended_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX support_sessions_tenant ON support_sessions (tenant_id);

-- operator-side rollouts (§15) — platform.flags.manage only.
CREATE TABLE feature_flags (
  key text PRIMARY KEY,
  enabled boolean NOT NULL DEFAULT false,
  rollout jsonb NOT NULL DEFAULT '{}',       -- {tenant_ids[], percent} later
  updated_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO feature_flags (key, enabled) VALUES
  ('platform.ai_assistant.enabled', false),
  ('platform.elections.enabled', false),
  ('platform.welfare_pro.enabled', false);
