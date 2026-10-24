-- Ref: oga9-engineering/sections/10-organization-core.md (group 8, ADR-022)
CREATE TABLE custom_fields (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  group_id uuid REFERENCES custom_field_groups(id) ON DELETE SET NULL,
  organization_id uuid NOT NULL REFERENCES organizations(id),
  entity_type text NOT NULL,
  key text NOT NULL,                          -- 'graduation_year'
  label text NOT NULL, help_text text,
  kind text NOT NULL DEFAULT 'text',          -- text|textarea|number|money|date|datetime|boolean|select|multiselect|file|member_ref|url|phone|email
  required boolean NOT NULL DEFAULT false,
  options jsonb,                              -- select options [{value,label}]
  validation jsonb NOT NULL DEFAULT '{}',     -- {min,max,pattern}
  sort_order int NOT NULL DEFAULT 0,
  visibility text NOT NULL DEFAULT 'admin',
  source text NOT NULL DEFAULT 'custom',
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, entity_type, key)
);
