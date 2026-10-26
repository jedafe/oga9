-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — voice rooms.

-- name: CreateVoiceRoom :one
INSERT INTO voice_rooms (organization_id, channel_id, activity_id, title, provider, room_name, started_by)
VALUES (sqlc.arg(organization_id), sqlc.narg(channel_id)::uuid,
  sqlc.narg(activity_id)::uuid, sqlc.narg(title)::text,
  sqlc.arg(provider), sqlc.arg(room_name), sqlc.arg(started_by))
RETURNING *;

-- name: GetVoiceRoom :one
SELECT * FROM voice_rooms WHERE id = $1 AND organization_id = $2;

-- name: EndVoiceRoom :execrows
UPDATE voice_rooms SET state = 'ended', ended_at = now()
WHERE id = $1 AND organization_id = $2 AND state = 'live';

-- name: JoinVoiceRoom :exec
INSERT INTO voice_room_participants (room_id, membership_id)
VALUES ($1, $2)
ON CONFLICT (room_id, membership_id, joined_at) DO NOTHING;

-- name: LeaveVoiceRoom :execrows
UPDATE voice_room_participants SET left_at = now()
WHERE room_id = $1 AND membership_id = $2 AND left_at IS NULL;

-- name: VoiceRoomParticipants :many
SELECT * FROM voice_room_participants WHERE room_id = $1 ORDER BY joined_at;

-- name: LiveVoiceRooms :many
SELECT * FROM voice_rooms WHERE organization_id = $1 AND state = 'live'
ORDER BY started_at DESC;

-- name: StaleLiveVoiceRooms :many
-- voice:room_sweep — rooms abandoned 'live' (crash, never ended) would
-- keep accepting joiners; the sweep closes them past the age cap.
SELECT * FROM voice_rooms WHERE state = 'live'
  AND started_at < now() - make_interval(hours => $1);

-- name: EndVoiceRoomParticipants :exec
-- room end stamps every still-open participant row.
UPDATE voice_room_participants SET left_at = now() WHERE room_id = $1 AND left_at IS NULL;
