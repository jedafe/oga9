-- Ref: oga9-engineering/sections/20-projects.md

CREATE TABLE projects (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  title text NOT NULL, description text,
  resolution_id uuid REFERENCES resolutions(id),   -- approval edge (passed resolution only)
  state text NOT NULL DEFAULT 'proposed',          -- proposed|approved|active|suspended|completed|abandoned
  budget_id uuid,
  unit_id uuid REFERENCES org_units(id),           -- owning committee
  starts_on date, target_on date, completed_on date,
  progress_pct int NOT NULL DEFAULT 0,             -- milestone-weighted, recomputed on completion
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX projects_org ON projects (organization_id, state);

CREATE TABLE project_milestones (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id),
  title text NOT NULL, weight int NOT NULL DEFAULT 1,
  due_on date, completed_on date, state text NOT NULL DEFAULT 'pending',
  UNIQUE(project_id, title)
);
CREATE INDEX project_milestones_project ON project_milestones (project_id);

CREATE TABLE project_updates (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id),
  author_id uuid NOT NULL REFERENCES organization_memberships(id),
  body text NOT NULL, progress_pct int,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX project_updates_project ON project_updates (project_id, created_at DESC);

CREATE TABLE vendors (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  name text NOT NULL, contact jsonb, kind text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX vendors_org ON vendors (organization_id);

CREATE TABLE project_donors (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  project_id uuid NOT NULL REFERENCES projects(id),
  donor_name text NOT NULL, membership_id uuid REFERENCES organization_memberships(id),
  amount_minor bigint, currency text, payment_id uuid,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX project_donors_project ON project_donors (project_id);

-- 'projects' app: resolution-gated delivery (requires governance +
-- finance_lite for budget/donor edges).
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('projects','1.0.0','Projects',false,'{
  "id":"projects","version":"1.0.0","requires":["organization_core","members","governance","finance_lite"],
  "permissions":["org.projects.propose","org.projects.approve","org.projects.manage","org.projects.update","org.projects.view","org.vendors.manage"],
  "events":{"publishes":["ProjectProposed","ProjectApproved","ProjectStarted","ProjectSuspended","MilestoneCompleted","ProjectUpdatePublished","VendorEngaged","DonorRecorded","ProjectClosed"],"consumes":["ResolutionPassed"]},
  "entitlements":{"feature":"app.projects"}
}');
