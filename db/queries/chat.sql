-- Ref: oga9-engineering/sections/24-realtime-chat-voice.md — chat.

-- name: CreateChannel :one
INSERT INTO chat_channels (organization_id, kind, unit_id, name, created_by)
VALUES (sqlc.arg(organization_id), sqlc.arg(kind), sqlc.narg(unit_id)::uuid,
  sqlc.narg(name)::text, sqlc.arg(created_by))
ON CONFLICT DO NOTHING   -- the partial functional unique can't be named
RETURNING *;

-- name: GetChannelByKind :one
-- the general/exco/unit channel for an org — one row by construction.
SELECT * FROM chat_channels
WHERE organization_id = sqlc.arg(organization_id) AND kind = sqlc.arg(kind)
  AND (sqlc.narg(unit_id)::uuid IS NULL AND unit_id IS NULL
       OR unit_id = sqlc.narg(unit_id)::uuid)
  AND state = 'active'
ORDER BY created_at LIMIT 1;

-- name: GetChannel :one
SELECT * FROM chat_channels WHERE id = $1 AND organization_id = $2 AND state = 'active';

-- name: GetChannelAny :one
SELECT * FROM chat_channels WHERE id = $1;

-- name: ChannelsForMember :many
-- visible channels: general (all members) + derived exco/committee seats +
-- home-unit channel + explicit rows (custom/dm). The join also binds the
-- membership to the channel's org (Rule 24 — cross-org seats can't exist).
SELECT c.* FROM chat_channels c
JOIN organization_memberships m ON m.id = $2 AND m.organization_id = c.organization_id
WHERE c.organization_id = $1 AND c.state = 'active' AND (
  c.kind = 'general'
  OR (c.kind = 'unit' AND c.unit_id = m.org_unit_id)
  OR (c.kind = 'exco' AND EXISTS (SELECT 1 FROM officer_appointments oa
    WHERE oa.member_id = m.id AND (oa.ends_at IS NULL OR oa.ends_at >= CURRENT_DATE)))
  OR (c.kind = 'committee' AND EXISTS (SELECT 1 FROM committee_memberships cmm
    WHERE cmm.member_id = m.id AND cmm.org_unit_id = c.unit_id))
  OR EXISTS (SELECT 1 FROM chat_channel_members cm
    WHERE cm.channel_id = c.id AND cm.membership_id = m.id)
)
ORDER BY c.last_message_at DESC NULLS LAST, c.created_at DESC;

-- name: ChannelMember :one
SELECT * FROM chat_channel_members WHERE channel_id = $1 AND membership_id = $2;

-- name: IsChannelMember :one
-- implicit membership: general channel = every active member; unit channel
-- = home-unit members; exco/committee derive from appointments/committee
-- rows; explicit rows cover custom/dm (+ seats added on committee).
-- The join binds the membership to the channel's org — a foreign
-- membership id must never satisfy this check (cross-org seats don't
-- exist, Rule 24).
SELECT true FROM chat_channels c
JOIN organization_memberships m ON m.id = $2 AND m.organization_id = c.organization_id
WHERE c.id = $1 AND (
  c.kind = 'general'
  OR (c.kind = 'unit' AND c.unit_id = m.org_unit_id)
  OR (c.kind = 'exco' AND EXISTS (SELECT 1 FROM officer_appointments oa
    WHERE oa.member_id = m.id AND (oa.ends_at IS NULL OR oa.ends_at >= CURRENT_DATE)))
  OR (c.kind = 'committee' AND EXISTS (SELECT 1 FROM committee_memberships cmm
    WHERE cmm.member_id = m.id AND cmm.org_unit_id = c.unit_id))
  OR EXISTS (SELECT 1 FROM chat_channel_members cm WHERE cm.channel_id = c.id AND cm.membership_id = m.id)
);

-- name: JoinChannel :exec
INSERT INTO chat_channel_members (channel_id, membership_id, role)
VALUES ($1, $2, COALESCE(NULLIF($3, ''), 'member'))
ON CONFLICT (channel_id, membership_id) DO NOTHING;

-- name: PostMessage :one
INSERT INTO chat_messages (channel_id, membership_id, kind, body, media_id, voice_duration_ms, reply_to_id)
VALUES (sqlc.arg(channel_id), sqlc.arg(membership_id), sqlc.arg(kind),
  sqlc.narg(body)::text, sqlc.narg(media_id)::uuid,
  sqlc.narg(voice_duration_ms)::int, sqlc.narg(reply_to_id)::uuid)
RETURNING *;

-- name: TouchChannel :exec
UPDATE chat_channels SET last_message_at = now() WHERE id = $1;

-- name: ChannelMessages :many
SELECT * FROM chat_messages WHERE channel_id = $1 AND deleted_at IS NULL
  AND ($2::timestamptz IS NULL OR created_at < $2)
ORDER BY created_at DESC LIMIT 50;

-- name: GetMessage :one
SELECT * FROM chat_messages WHERE id = $1 AND channel_id = $2 AND deleted_at IS NULL;

-- name: SoftDeleteMessage :execrows
UPDATE chat_messages SET deleted_at = now() WHERE id = $1 AND channel_id = $2 AND deleted_at IS NULL;

-- name: MarkChannelRead :exec
INSERT INTO chat_read_states (channel_id, membership_id, last_read_message_id, updated_at)
VALUES ($1, $2, $3, now())
ON CONFLICT (channel_id, membership_id)
DO UPDATE SET last_read_message_id = EXCLUDED.last_read_message_id, updated_at = now();

-- name: MuteChannel :exec
-- mute lives on chat_read_states, never chat_channel_members: a members row
-- is a SEAT, and on derived channels (exco/unit/committee) a minted seat
-- would outlive the appointment/unit it derived from (§24).
INSERT INTO chat_read_states (channel_id, membership_id, muted_until)
VALUES ($1, $2, $3)
ON CONFLICT (channel_id, membership_id) DO UPDATE SET muted_until = EXCLUDED.muted_until;

-- name: ChannelMemberIDs :many
-- explicit members + (general: all active) + (unit: home-unit members) —
-- the notification fan-out audience for a channel message.
SELECT DISTINCT m.id FROM organization_memberships m
JOIN chat_channels c ON c.id = $1 AND c.organization_id = m.organization_id
WHERE m.lifecycle_state IN ('active','life_member') AND (
  c.kind = 'general'
  OR (c.kind = 'unit' AND m.org_unit_id = c.unit_id)
  OR (c.kind = 'exco' AND EXISTS (SELECT 1 FROM officer_appointments oa
    WHERE oa.member_id = m.id AND (oa.ends_at IS NULL OR oa.ends_at >= CURRENT_DATE)))
  OR (c.kind = 'committee' AND EXISTS (SELECT 1 FROM committee_memberships cmm
    WHERE cmm.member_id = m.id AND cmm.org_unit_id = c.unit_id))
  OR EXISTS (SELECT 1 FROM chat_channel_members cm WHERE cm.channel_id = c.id AND cm.membership_id = m.id)
);

-- name: IsActiveOfficer :one
-- §24 exco scope: an active term-scoped appointment (Rule 21) counts.
SELECT EXISTS (SELECT 1 FROM officer_appointments
  WHERE member_id = $1 AND (ends_at IS NULL OR ends_at >= CURRENT_DATE));

-- name: EditMessage :execrows
UPDATE chat_messages SET body = $3, edited_at = now()
WHERE id = $1 AND channel_id = $2 AND membership_id = $4 AND deleted_at IS NULL;

-- name: IsChannelMemberForUser :one
-- WS subscribe proof: the channel AND the membership must check out, the
-- membership must belong to the session's user, AND it must live in the
-- channel's org — a client-supplied membership_id can't borrow someone
-- else's seat or another org's channel.
SELECT true FROM chat_channels c
JOIN organization_memberships m ON m.id = $2 AND m.user_id = $3
  AND m.organization_id = c.organization_id
WHERE c.id = $1 AND (
  c.kind = 'general'
  OR (c.kind = 'unit' AND c.unit_id = m.org_unit_id)
  OR (c.kind = 'exco' AND EXISTS (SELECT 1 FROM officer_appointments oa
    WHERE oa.member_id = m.id AND (oa.ends_at IS NULL OR oa.ends_at >= CURRENT_DATE)))
  OR (c.kind = 'committee' AND EXISTS (SELECT 1 FROM committee_memberships cmm
    WHERE cmm.member_id = m.id AND cmm.org_unit_id = c.unit_id))
  OR EXISTS (SELECT 1 FROM chat_channel_members cm WHERE cm.channel_id = c.id AND cm.membership_id = m.id)
);

-- name: ArchiveChannel :execrows
UPDATE chat_channels SET state = 'archived'
WHERE id = $1 AND organization_id = $2 AND state = 'active';

-- name: RemoveChannelMember :execrows
DELETE FROM chat_channel_members WHERE channel_id = $1 AND membership_id = $2;

-- name: LockChannel :one
-- Serialize member-list mutations on the channel row (dm 2-seat cap) —
-- two concurrent adds must not both pass the headcount.
SELECT id FROM chat_channels WHERE id = $1 FOR UPDATE;

-- name: RetentionSweepChat :many
-- chat:retention_sweep — org.chat.retention_days > 0 → messages past the
-- window soft-delete (Rule 11; hard erase stays with the erasure task).
UPDATE chat_messages SET deleted_at = now()
WHERE deleted_at IS NULL
  AND channel_id IN (
    SELECT c.id FROM chat_channels c
    JOIN org_settings s ON s.organization_id = c.organization_id
    WHERE s.key = 'org.chat.retention_days'
      AND jsonb_typeof(s.value) = 'number'
      AND (s.value #>> '{}')::int > 0
      AND chat_messages.created_at < now() - make_interval(days => (s.value #>> '{}')::int))
RETURNING id, (SELECT c.organization_id FROM chat_channels c WHERE c.id = chat_messages.channel_id) AS organization_id;
