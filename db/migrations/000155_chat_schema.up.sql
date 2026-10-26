-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — chat schema
-- (g11 range; voice_rooms are post-MVP and land with LiveKit).
CREATE TABLE chat_channels (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  kind text NOT NULL,                     -- general|exco|unit|committee|custom|dm
  unit_id uuid REFERENCES org_units(id),  -- when kind=unit|committee
  name text,                              -- required unless general
  created_by uuid REFERENCES organization_memberships(id),
  state text NOT NULL DEFAULT 'active',   -- active|archived
  last_message_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, kind, unit_id)  -- one general/exco/unit channel
);
CREATE TABLE chat_channel_members (
  channel_id uuid NOT NULL REFERENCES chat_channels(id) ON DELETE CASCADE,
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  role text NOT NULL DEFAULT 'member',    -- owner|moderator|member
  muted_until timestamptz,
  joined_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (channel_id, membership_id)
);
CREATE TABLE chat_messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  channel_id uuid NOT NULL REFERENCES chat_channels(id) ON DELETE CASCADE,
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  kind text NOT NULL DEFAULT 'text',      -- text|voice|file|system
  body text,                              -- null for voice/file
  media_id uuid REFERENCES documents(id), -- voice note/file via documents
  voice_duration_ms int,
  reply_to_id uuid REFERENCES chat_messages(id),
  edited_at timestamptz, deleted_at timestamptz,   -- soft delete, Rule 11
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX chat_messages_channel ON chat_messages (channel_id, created_at DESC)
  WHERE deleted_at IS NULL;
CREATE TABLE chat_read_states (
  channel_id uuid NOT NULL REFERENCES chat_channels(id) ON DELETE CASCADE,
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  last_read_message_id uuid REFERENCES chat_messages(id),
  updated_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (channel_id, membership_id)
);
