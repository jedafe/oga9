-- Ref: oga9-engineering/sections/23-integrations.md

CREATE TABLE connectors (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id),  -- null = platform connector
  kind text NOT NULL,                                  -- paystack|flutterwave|smtp|ics|japaconnect
  config jsonb NOT NULL DEFAULT '{}',                  -- non-secret config
  credential_ref text,                                 -- secrets-manager pointer ONLY
  enabled boolean NOT NULL DEFAULT true,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX connectors_org ON connectors (organization_id) WHERE organization_id IS NOT NULL;

CREATE TABLE webhook_receipts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider text NOT NULL, raw jsonb NOT NULL,
  signature_ok boolean NOT NULL,
  processed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX webhook_receipts_unprocessed ON webhook_receipts (created_at) WHERE processed_at IS NULL;

CREATE TABLE sync_jobs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  connector_id uuid NOT NULL REFERENCES connectors(id),
  direction text NOT NULL,                             -- push|pull
  cursor text, state text NOT NULL DEFAULT 'queued',
  detail jsonb, ran_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sync_jobs_pending ON sync_jobs (created_at) WHERE state = 'queued';

CREATE TABLE external_event_map (
  internal_type text NOT NULL, internal_id uuid NOT NULL,
  external_type text NOT NULL, external_id text NOT NULL,
  PRIMARY KEY (internal_type, internal_id, external_type),
  UNIQUE(external_type, external_id)
);

CREATE TABLE api_keys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid REFERENCES tenants(id),
  user_id uuid REFERENCES users(id),                  -- null = tenant key
  prefix text NOT NULL, hashed_key text NOT NULL UNIQUE,
  scopes text[] NOT NULL DEFAULT '{}', rate_tier text NOT NULL DEFAULT 'standard',
  revoked_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX api_keys_tenant ON api_keys (tenant_id) WHERE revoked_at IS NULL;

-- 'integrations' app: connectors + webhook intake (requires
-- organization_core only).
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('integrations','1.0.0','Integrations',false,'{
  "id":"integrations","version":"1.0.0","requires":["organization_core"],
  "permissions":["org.integrations.configure","org.integrations.view","platform.integrations.manage","api.keys.self_manage"],
  "events":{"publishes":["ConnectorConfigured","WebhookReceived","ExternalEventReceived","SyncCompleted","SyncFailed","OrganizationSynced","CredentialRotated","MembershipRequestReceived"],"consumes":["OrganizationPublished","OrganizationLocationUpdated"]},
  "entitlements":{"feature":"app.integrations"}
}');
