-- Ref: oga9-engineering/sections/04-tenancy-organizations.md
-- NULL parent_unit_id doesn't participate in the UNIQUE(org,parent,slug)
-- constraint — two root units could share a slug. Partial index closes it.
CREATE UNIQUE INDEX org_units_root_slug ON org_units (organization_id, slug)
  WHERE parent_unit_id IS NULL;
