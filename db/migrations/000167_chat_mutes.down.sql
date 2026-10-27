ALTER TABLE chat_channel_members ADD COLUMN muted_until timestamptz;
UPDATE chat_channel_members cm SET muted_until = rs.muted_until
FROM chat_read_states rs
WHERE rs.channel_id = cm.channel_id AND rs.membership_id = cm.membership_id
  AND rs.muted_until IS NOT NULL;
ALTER TABLE chat_read_states DROP COLUMN muted_until;
