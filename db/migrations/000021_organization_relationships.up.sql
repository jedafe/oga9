-- Ref: oga9-engineering/sections/04-tenancy-organizations.md
CREATE TABLE organization_relationships (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  related_org_id uuid NOT NULL REFERENCES organizations(id),
  relationship text NOT NULL,               -- parent|affiliate|federation|chapter
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, related_org_id, relationship)
);
