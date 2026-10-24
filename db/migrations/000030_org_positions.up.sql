-- Ref: oga9-engineering/sections/03-rbac-positions.md (group 5)
CREATE TABLE org_positions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  name text NOT NULL, slug text NOT NULL,
  is_executive boolean NOT NULL DEFAULT false,
  seats int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, slug)
);
