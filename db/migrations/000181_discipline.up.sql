-- Ref: oga9-engineering/sections/16-governance.md — discipline & grievance
-- casework: case workflow investigation → hearing → resolution → appeal →
-- fine/sanction. Fines post through §12 via the finance seam (never off-ledger).

CREATE TABLE discipline_cases (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  subject_id uuid NOT NULL REFERENCES organization_memberships(id),
  reported_by uuid REFERENCES organization_memberships(id),
  kind text NOT NULL,                            -- conduct|financial|attendance|other
  title text NOT NULL, details text,
  state text NOT NULL DEFAULT 'open',            -- open|investigating|hearing|resolved|appealed|closed|dismissed
  sanction text,                                 -- none|warning|fine|suspension
  fine_minor bigint, currency text,
  resolved_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX discipline_cases_org ON discipline_cases (organization_id, state, created_at DESC);
CREATE INDEX discipline_cases_subject ON discipline_cases (subject_id);

CREATE TABLE discipline_hearings (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id uuid NOT NULL REFERENCES discipline_cases(id) ON DELETE CASCADE,
  scheduled_at timestamptz, held_at timestamptz,
  outcome text, notes text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX discipline_hearings_case ON discipline_hearings (case_id);

CREATE TABLE discipline_appeals (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id uuid NOT NULL REFERENCES discipline_cases(id) ON DELETE CASCADE,
  filed_by uuid NOT NULL REFERENCES organization_memberships(id),
  grounds text NOT NULL,
  state text NOT NULL DEFAULT 'filed',           -- filed|upheld|overturned
  decided_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX discipline_appeals_case ON discipline_appeals (case_id);

-- §16 permission nodes (permission-key-naming.md); org_admin inherits via
-- the org.% grant pattern from 000019.
INSERT INTO permissions (node, label) VALUES
('org.discipline.view','View discipline cases'),
('org.discipline.manage','Manage discipline cases'),
('org.discipline.hear','Hear and resolve discipline cases'),
('org.discipline.appeal_own','Appeal own discipline case');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'org_admin' AND r.tenant_id IS NULL AND p.node LIKE 'org.discipline.%';
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'member' AND r.tenant_id IS NULL AND p.node = 'org.discipline.appeal_own';

-- extend the governance app manifest with the discipline surface.
UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  (manifest::jsonb->'permissions') ||
  '["org.discipline.view","org.discipline.manage","org.discipline.hear","org.discipline.appeal_own"]'::jsonb)::json
WHERE app_id = 'governance';
