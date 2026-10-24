-- Ref: oga9-engineering/sections/04-tenancy-organizations.md (ADR-017)
CREATE TABLE organization_templates (
  slug text PRIMARY KEY,
  version int NOT NULL, label text NOT NULL,
  seed jsonb NOT NULL    -- positions, committees, contribution types, rules, field_groups, level_labels, theme
);
