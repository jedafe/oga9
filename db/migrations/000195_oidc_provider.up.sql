-- Ref: oga9-engineering/sections/29-wordpress-connector.md + ADR-028 —
-- OIDC/OAuth2 provider slice, generalized to two client kinds:
-- connector (org-scoped, e.g. WordPress member login) and platform
-- (no org context — ecosystem relying parties like japa.to).
CREATE TABLE oauth_clients (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  kind text NOT NULL,                                -- connector|platform
  connector_id uuid REFERENCES connectors(id),       -- connector kind only
  organization_id uuid REFERENCES organizations(id), -- connector kind only
  client_id text NOT NULL UNIQUE,                    -- oga9_wp_{ulid}|oga9_plt_{ulid}
  hashed_secret text NOT NULL,                       -- confidential clients only
  redirect_uris text[] NOT NULL DEFAULT '{}',
  name text NOT NULL,
  trusted boolean NOT NULL DEFAULT false,            -- first-party: skip consent
  status text NOT NULL DEFAULT 'active',             -- active|disabled
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz,
  CHECK (kind IN ('connector','platform')),
  CHECK (status IN ('active','disabled')),
  CHECK ((kind = 'platform' AND organization_id IS NULL AND connector_id IS NULL)
      OR (kind = 'connector' AND organization_id IS NOT NULL AND connector_id IS NOT NULL))
);

CREATE TABLE oauth_authorization_codes (
  code text PRIMARY KEY,                             -- high-entropy, single-use
  client_id text NOT NULL REFERENCES oauth_clients(client_id),
  user_id uuid NOT NULL REFERENCES users(id),
  organization_id uuid REFERENCES organizations(id), -- null on platform clients
  redirect_uri text NOT NULL,
  code_challenge text NOT NULL,
  code_challenge_method text NOT NULL DEFAULT 'S256',
  expires_at timestamptz NOT NULL,                   -- 120 s
  consumed_at timestamptz
);
CREATE INDEX oauth_codes_user ON oauth_authorization_codes (user_id, client_id);

CREATE TABLE oauth_grants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  client_id text NOT NULL REFERENCES oauth_clients(client_id),
  user_id uuid NOT NULL REFERENCES users(id),
  organization_id uuid REFERENCES organizations(id), -- null on platform clients
  session_id uuid REFERENCES user_sessions(id),      -- underlying OGA9 session (§02)
  refresh_token_hash text,                           -- opaque 256-bit, hashed
  refresh_expires_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
);
CREATE INDEX oauth_grants_user ON oauth_grants (user_id) WHERE revoked_at IS NULL;
CREATE INDEX oauth_grants_refresh ON oauth_grants (refresh_token_hash) WHERE revoked_at IS NULL;
CREATE INDEX oauth_grants_session ON oauth_grants (session_id) WHERE revoked_at IS NULL;

INSERT INTO permissions (node, label) VALUES
('platform.clients.manage','Manage platform OAuth/OIDC clients (ecosystem relying parties)')
ON CONFLICT (node) DO NOTHING;

INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'platform.clients.manage' FROM user_roles r
WHERE r.slug = 'platform_admin' AND r.tenant_id IS NULL
ON CONFLICT DO NOTHING;
