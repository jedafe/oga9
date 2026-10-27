-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — muted_until on
-- chat_channel_members minted an EXPLICIT seat on derived channels
-- (exco/unit/committee): the row survived the loss of derived membership,
-- so an ex-officer who once muted the exco channel kept access after term
-- end. Mute state moves to chat_read_states (same channel+member key,
-- never confers a seat).
ALTER TABLE chat_read_states ADD COLUMN muted_until timestamptz;
INSERT INTO chat_read_states (channel_id, membership_id, muted_until)
SELECT channel_id, membership_id, muted_until FROM chat_channel_members
WHERE muted_until IS NOT NULL
ON CONFLICT (channel_id, membership_id)
DO UPDATE SET muted_until = EXCLUDED.muted_until;
-- existing explicit rows minted only by mute are indistinguishable from
-- real seats; leave them (they grant nothing extra on derived kinds once
-- mute moves — see IsChannelMember).
ALTER TABLE chat_channel_members DROP COLUMN muted_until;
