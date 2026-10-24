-- Ref: oga9-engineering/sections/03-rbac-positions.md (group 5)
CREATE TABLE committee_memberships (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  org_unit_id uuid NOT NULL REFERENCES org_units(id),
  member_id uuid NOT NULL REFERENCES organization_memberships(id),
  role text NOT NULL DEFAULT 'member',
  created_at timestamptz NOT NULL DEFAULT now()
);
