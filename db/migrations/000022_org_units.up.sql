-- Ref: oga9-engineering/sections/04-tenancy-organizations.md
CREATE TABLE org_units (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  parent_unit_id uuid REFERENCES org_units(id),
  type text NOT NULL,                       -- hq|branch|chapter|committee|wing
  level_label text,                         -- org-defined tier: 'country'|'state'|'local'...
  name text NOT NULL, slug text NOT NULL,
  archived_at timestamptz,                  -- §10 archive semantics (never hard-delete)
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, parent_unit_id, slug)
);
CREATE INDEX org_units_org ON org_units (organization_id) WHERE archived_at IS NULL;
