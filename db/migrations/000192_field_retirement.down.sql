DROP INDEX IF EXISTS custom_fields_org_entity_key;
ALTER TABLE custom_fields ADD CONSTRAINT custom_fields_organization_id_entity_type_key_key
  UNIQUE (organization_id, entity_type, key);
ALTER TABLE custom_fields DROP COLUMN retired_at;
