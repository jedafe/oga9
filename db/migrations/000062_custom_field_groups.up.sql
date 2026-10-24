-- Ref: oga9-engineering/sections/10-organization-core.md (group 8, ADR-022)
CREATE TABLE custom_field_groups (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  entity_type text NOT NULL,                  -- member|application|organization|event|welfare_case|asset|project
  name text NOT NULL, slug text NOT NULL,
  sort_order int NOT NULL DEFAULT 0,
  visibility text NOT NULL DEFAULT 'admin',   -- member|exco|admin|public
  source text NOT NULL DEFAULT 'custom',      -- template|custom
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, entity_type, slug)
);
