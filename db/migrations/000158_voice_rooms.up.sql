-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — voice rooms
-- (room state lives server-side; LiveKit carries only media).
CREATE TABLE voice_rooms (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  channel_id uuid REFERENCES chat_channels(id),      -- spawned from channel
  activity_id uuid REFERENCES activities(id),        -- or linked to meeting
  title text, provider text NOT NULL DEFAULT 'livekit',
  room_name text NOT NULL UNIQUE,
  started_by uuid NOT NULL REFERENCES organization_memberships(id),
  state text NOT NULL DEFAULT 'live',                -- live|ended
  started_at timestamptz NOT NULL DEFAULT now(), ended_at timestamptz
);
CREATE TABLE voice_room_participants (
  room_id uuid NOT NULL REFERENCES voice_rooms(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  joined_at timestamptz NOT NULL DEFAULT now(), left_at timestamptz,
  muted_by uuid REFERENCES organization_memberships(id),
  PRIMARY KEY (room_id, membership_id, joined_at)
);
