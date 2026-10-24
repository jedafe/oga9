-- Ref: oga9-engineering/sections/10-organization-core.md (group 8, ADR-022)
CREATE TABLE custom_field_values (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  field_id uuid NOT NULL REFERENCES custom_fields(id) ON DELETE CASCADE,
  organization_id uuid NOT NULL REFERENCES organizations(id),
  entity_id uuid NOT NULL,                    -- member|org|event|... id
  value jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(field_id, entity_id)
);
