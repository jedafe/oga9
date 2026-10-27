-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — the 000157 fix
-- was over-broad: COALESCE(unit_id, '0000…') collapsed ALL kinds onto one
-- row per (org,kind), so a second 'dm' or 'custom' channel hit the unique
-- index — and ON CONFLICT DO NOTHING silently returned the EXISTING
-- channel (misrouting a new DM into an unrelated conversation).
-- Scope uniqueness to the derived kinds only: one general/exco per org,
-- one unit/committee channel per unit. dm/custom stay unbounded.
DROP INDEX chat_channels_org_kind_unit;
CREATE UNIQUE INDEX chat_channels_org_kind_unit
  ON chat_channels (organization_id, kind, COALESCE(unit_id, '00000000-0000-0000-0000-000000000000'))
  WHERE state = 'active' AND kind IN ('general','exco','unit','committee');
