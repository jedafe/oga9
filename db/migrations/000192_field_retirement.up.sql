-- Ref: oga9-engineering/sections/10-organization-core.md — fields RETIRE,
-- they don't hard-delete (CustomFieldRetired event): custom_field_values
-- CASCADE-drops on field delete would silently destroy org data. Retired
-- fields keep their values; their key frees for reuse (same partial-unique
-- pattern as claim external_ref).
ALTER TABLE custom_fields ADD COLUMN retired_at timestamptz;
ALTER TABLE custom_fields DROP CONSTRAINT custom_fields_organization_id_entity_type_key_key;
CREATE UNIQUE INDEX custom_fields_org_entity_key ON custom_fields (organization_id, entity_type, key)
  WHERE retired_at IS NULL;
