-- Ref: oga9-engineering/sections/18-assets.md

CREATE TABLE assets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  category text NOT NULL,                        -- building|vehicle|equipment|inventory|digital|bank_account|domain|social_account|seal|archive
  name text NOT NULL, description text,
  identifier text,                               -- serial/plate/reg no
  condition text NOT NULL DEFAULT 'good',        -- new|good|fair|poor|disposed
  acquired_at date, cost_minor bigint, currency text,
  unit_id uuid REFERENCES org_units(id),         -- owning branch
  state text NOT NULL DEFAULT 'active',          -- active|in_repair|disposed|missing
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX assets_org ON assets (organization_id, state);

CREATE TABLE custody_assignments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_id uuid NOT NULL REFERENCES assets(id),
  holder_member_id uuid REFERENCES organization_memberships(id),  -- null = unit custody
  holder_unit_id uuid REFERENCES org_units(id),
  term_id uuid REFERENCES terms(id),              -- custody period (Rule 21)
  assigned_by uuid NOT NULL REFERENCES organization_memberships(id),
  assigned_at timestamptz NOT NULL DEFAULT now(),
  returned_at timestamptz
);
CREATE INDEX custody_asset ON custody_assignments (asset_id, returned_at NULLS FIRST);
CREATE INDEX custody_term ON custody_assignments (term_id) WHERE returned_at IS NULL;

CREATE TABLE maintenance_records (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_id uuid NOT NULL REFERENCES assets(id),
  kind text NOT NULL,                            -- service|repair|inspection
  detail text, cost_minor bigint, currency text,
  next_due date,
  logged_by uuid NOT NULL REFERENCES organization_memberships(id),
  logged_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX maintenance_due ON maintenance_records (next_due) WHERE next_due IS NOT NULL;

CREATE TABLE asset_audits (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  started_by uuid NOT NULL REFERENCES organization_memberships(id),
  findings jsonb,                                 -- [{asset_id,finding,note}]
  state text NOT NULL DEFAULT 'open',            -- open|completed
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE digital_credentials (                -- ownership refs, no secrets
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  asset_id uuid NOT NULL REFERENCES assets(id),
  kind text NOT NULL,                            -- domain|email|social|bank|other
  label text NOT NULL, reference text NOT NULL,
  vault_ref text,                                 -- secrets-manager pointer only
  updated_at timestamptz NOT NULL DEFAULT now()
);

-- 'assets' app: requires organization_core + members + governance
-- (custody is term-scoped).
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('assets','1.0.0','Assets',false,'{
  "id":"assets","version":"1.0.0","requires":["organization_core","members","governance"],
  "permissions":["org.assets.register","org.assets.assign","org.assets.maintain","org.assets.dispose","org.assets.audit","org.assets.view","org.assets.credentials.manage"],
  "events":{"publishes":["AssetRegistered","AssetAssigned","AssetReturned","AssetMaintenanceLogged","AssetDisposed","AssetAuditCompleted","CustodyHandoverFlagged","MaintenanceDue"],"consumes":["TermEnded"]},
  "entitlements":{"feature":"app.assets"}
}');
