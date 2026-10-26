-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — the declared
-- UNIQUE(organization_id, kind, unit_id) never fires when unit_id IS NULL
-- (NULLs are distinct): a second 'general'/'exco' channel was creatable.
-- Enforce it with a COALESCE functional unique.
ALTER TABLE chat_channels DROP CONSTRAINT IF EXISTS chat_channels_organization_id_kind_unit_id_key;
CREATE UNIQUE INDEX chat_channels_org_kind_unit
  ON chat_channels (organization_id, kind, COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'))
  WHERE state = 'active';
