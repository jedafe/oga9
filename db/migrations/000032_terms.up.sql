-- Ref: oga9-engineering/sections/03-rbac-positions.md (group 5)
CREATE TABLE terms (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  label text NOT NULL,
  starts_at date NOT NULL, ends_at date NOT NULL,
  is_current boolean NOT NULL DEFAULT false
);
-- exactly one current term per org; closed terms stay (is_current=false rows)
CREATE UNIQUE INDEX one_current_term ON terms(organization_id) WHERE is_current;
