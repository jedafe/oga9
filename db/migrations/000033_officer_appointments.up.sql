-- Ref: oga9-engineering/sections/03-rbac-positions.md (group 5)
-- Term-scoped appointments (Rule 21). branch_id/committee_id carry scope.
CREATE TABLE officer_appointments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  term_id uuid NOT NULL REFERENCES terms(id),
  position_id uuid NOT NULL REFERENCES org_positions(id),
  member_id uuid NOT NULL REFERENCES organization_memberships(id),
  branch_id uuid REFERENCES org_units(id),
  committee_id uuid REFERENCES org_units(id),
  starts_at date NOT NULL, ends_at date,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX officer_appts_member ON officer_appointments (member_id, term_id);
