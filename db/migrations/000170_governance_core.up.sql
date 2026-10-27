-- Ref: oga9-engineering/sections/16-governance.md — motions → resolutions,
-- constitution versioning, committee mandates. terms/org_positions/
-- officer_appointments/committee_memberships already exist (§03, Phase 04).

CREATE TABLE motions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  activity_id uuid REFERENCES activities(id),      -- tabled at meeting
  mover_id uuid NOT NULL REFERENCES organization_memberships(id),
  seconder_id uuid REFERENCES organization_memberships(id),
  title text NOT NULL, body text NOT NULL,
  state text NOT NULL DEFAULT 'tabled',             -- tabled|debated|voted|withdrawn
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX motions_org ON motions (organization_id, created_at DESC);

CREATE TABLE resolutions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  motion_id uuid REFERENCES motions(id),
  activity_id uuid REFERENCES activities(id),       -- decided at meeting
  term_id uuid NOT NULL REFERENCES terms(id),       -- term-scoped, Rule 21
  title text NOT NULL, body text NOT NULL,
  outcome text NOT NULL,                            -- passed|rejected|amended_passed
  votes_for int, votes_against int, votes_abstain int,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX resolutions_org ON resolutions (organization_id, created_at DESC);

CREATE TABLE constitution_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  version int NOT NULL, body text NOT NULL,
  adopted_by_resolution_id uuid REFERENCES resolutions(id),
  adopted_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, version)
);
CREATE INDEX constitution_versions_org ON constitution_versions (organization_id, adopted_at DESC);

CREATE TABLE committee_mandates (
  org_unit_id uuid PRIMARY KEY REFERENCES org_units(id),  -- committees only
  mandate text NOT NULL, reports_to uuid REFERENCES org_units(id),
  budget_limit_minor bigint, currency text,
  active boolean NOT NULL DEFAULT true
);

-- 'governance' app: requires organization_core + members; the
-- app.governance entitlement gates activation (Rule 25, professional+).
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('governance','1.0.0','Governance',false,'{
  "id":"governance","version":"1.0.0","requires":["organization_core","members"],
  "permissions":["org.governance.view","org.governance.positions.manage","org.governance.terms.manage","org.governance.appointments.manage","org.governance.committees.manage","org.governance.resolutions.record","org.governance.constitution.manage"],
  "events":{"publishes":["TermStarted","TermEnded","LeaderAppointed","AppointmentEnded","CommitteeCreated","CommitteeDissolved","MotionTabled","ResolutionPassed","ResolutionRejected","ConstitutionAdopted","ConstitutionAmended","HandoverGenerated","HandoverCompleted"],"consumes":["MemberApproved","MemberSuspended","QuorumEvaluated"]},
  "entitlements":{"feature":"app.governance"}
}');
