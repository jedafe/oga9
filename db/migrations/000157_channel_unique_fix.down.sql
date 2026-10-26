DROP INDEX chat_channels_org_kind_unit;
ALTER TABLE chat_channels ADD CONSTRAINT chat_channels_organization_id_kind_unit_id_key
  UNIQUE (organization_id, kind, unit_id);
