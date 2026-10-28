-- Ref: oga9-engineering/sections/27-volunteers.md (group 25) — named
-- volunteer roles on an activity + member signups through
-- confirm→complete. Signups are never deleted (audit trail); a role
-- DELETE cascades only while it has no confirmed signups.

CREATE TABLE volunteer_roles (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  activity_id uuid NOT NULL REFERENCES activities(id) ON DELETE CASCADE,
  unit_id uuid REFERENCES org_units(id),       -- restrict eligibility to a committee/wing
  name text NOT NULL,                          -- usher|protocol|media|logistics|security|custom
  slots int NOT NULL DEFAULT 1,
  note text,
  created_by uuid REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX volunteer_roles_activity ON volunteer_roles (activity_id);
CREATE INDEX volunteer_roles_org ON volunteer_roles (organization_id);

CREATE TABLE volunteer_signups (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  role_id uuid NOT NULL REFERENCES volunteer_roles(id) ON DELETE CASCADE,
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  state text NOT NULL DEFAULT 'signed_up',     -- signed_up|confirmed|completed|no_show|cancelled
  note text,
  decided_by uuid REFERENCES organization_memberships(id), -- who confirmed/rejected
  signed_up_at timestamptz NOT NULL DEFAULT now(),
  completed_at timestamptz,
  UNIQUE (role_id, membership_id)
);
CREATE INDEX volunteer_signups_role ON volunteer_signups (role_id, state);
CREATE INDEX volunteer_signups_member ON volunteer_signups (membership_id);

-- §27 permission nodes.
INSERT INTO permissions (node, label) VALUES
('org.volunteers.view','View volunteer roster'),
('org.volunteers.signup_own','Sign up to volunteer'),
('org.volunteers.manage','Manage volunteer roles');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'org_admin' AND r.tenant_id IS NULL AND p.node IN (
  'org.volunteers.view','org.volunteers.signup_own','org.volunteers.manage');
-- catalog: view/signup_own → members; manage → activities officers (admins).
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'member' AND r.tenant_id IS NULL
  AND p.node IN ('org.volunteers.view','org.volunteers.signup_own');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'secretary' AND r.tenant_id IS NULL
  AND p.node = 'org.volunteers.manage';

UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  (manifest::jsonb->'permissions') ||
  '["org.volunteers.view","org.volunteers.signup_own","org.volunteers.manage"]'::jsonb)::json
WHERE app_id = 'events_meetings';
