DROP INDEX chat_channels_org_kind_unit;
CREATE UNIQUE INDEX chat_channels_org_kind_unit
  ON chat_channels (organization_id, kind, COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'))
  WHERE state = 'active';
