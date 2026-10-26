-- Ref: oga9-engineering/sections/14-comms-documents.md — comms queries.
-- Org-scoped everywhere (Rule 24); notifications are user-scoped for /me.

-- name: CreateAnnouncement :one
INSERT INTO announcements (organization_id, author_id, title, body, audience, state, published_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(author_id), sqlc.arg(title),
  sqlc.arg(body), sqlc.arg(audience), sqlc.arg(state),
  CASE WHEN sqlc.arg(state)::text = 'published' THEN now() ELSE NULL END)
RETURNING *;

-- name: ListAnnouncements :many
SELECT * FROM announcements WHERE organization_id = $1
  AND ($2::text = '' OR state = $2)
ORDER BY created_at DESC LIMIT 200;

-- name: GetAnnouncement :one
SELECT * FROM announcements WHERE id = $1;

-- name: RetractAnnouncement :one
UPDATE announcements SET state = 'retracted'
WHERE id = $1 AND organization_id = $2 AND state <> 'retracted'
RETURNING *;

-- name: AudienceUserIDs :many
-- Announcement audience snapshot: {"all":true} or {categories:[],unit_ids:[]}
-- resolve to active/life members' user ids at dispatch time.
SELECT DISTINCT m.user_id FROM organization_memberships m
WHERE m.organization_id = $1 AND m.lifecycle_state IN ('active','life_member')
  AND (sqlc.arg(all_members)::bool
       OR m.category = ANY(sqlc.arg(categories)::text[])
       OR m.org_unit_id = ANY(sqlc.arg(unit_ids)::uuid[]));

-- name: CreateNotification :one
INSERT INTO notifications (user_id, organization_id, kind, title, body, ref_type, ref_id, channel, delivered_at)
VALUES (sqlc.arg(user_id), sqlc.narg(organization_id)::uuid, sqlc.arg(kind),
  sqlc.arg(title), sqlc.narg(body)::text, sqlc.narg(ref_type)::text,
  sqlc.narg(ref_id)::uuid, sqlc.arg(channel),
  CASE WHEN sqlc.arg(channel)::text = 'in_app' THEN now() END) -- in_app = delivered at insert
RETURNING *;

-- name: NotificationWanted :one
-- preference gate: absent row = default-enabled.
SELECT COALESCE((SELECT enabled FROM notification_preferences
  WHERE user_id = $1 AND kind = $2 AND channel = $3), true);

-- name: MyNotifications :many
SELECT * FROM notifications WHERE user_id = $1
  AND ($2::bool = false OR read_at IS NULL)
ORDER BY created_at DESC LIMIT 200;

-- name: MarkNotificationRead :execrows
UPDATE notifications SET read_at = now()
WHERE id = $1 AND user_id = $2 AND read_at IS NULL;

-- name: MyPreferences :many
SELECT * FROM notification_preferences WHERE user_id = $1;

-- name: UpsertPreference :exec
INSERT INTO notification_preferences (user_id, kind, channel, enabled)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, kind, channel) DO UPDATE SET enabled = EXCLUDED.enabled;

-- name: GetNotification :one
SELECT * FROM notifications WHERE id = $1 AND user_id = $2;

-- name: NotificationExists :one
-- dispatch dedupe: a retried fan-out must not re-insert the same
-- (user, kind, ref) row — at-least-once tasks stay idempotent (Rule 27).
SELECT EXISTS (SELECT 1 FROM notifications
  WHERE user_id = $1 AND kind = $2 AND ref_id = $3 AND channel = $4);

-- name: GetNotificationChannel :one
-- email-channel dispatch state: the row's delivered_at decides whether a
-- retried fan-out re-enqueues (NULL = queued never landed → re-enqueue).
SELECT id, delivered_at FROM notifications
WHERE user_id = $1 AND kind = $2 AND ref_id = $3 AND channel = $4;

-- name: GetNotificationForDelivery :one
-- tenant_id joins through organizations — notifications has no tenant col.
SELECT n.id, n.organization_id, o.tenant_id FROM notifications n
JOIN organizations o ON o.id = n.organization_id WHERE n.id = $1;

-- name: MarkNotificationDelivered :exec
-- the notifications:email worker stamps delivery after the send succeeds.
UPDATE notifications SET delivered_at = now() WHERE id = $1;

-- name: AudienceRecipients :many
-- announcement fan-out audience with delivery addresses: same snapshot
-- as AudienceUserIDs + the member's user email for the email channel.
-- §14 audience spec: units/categories/positions — positions resolve
-- through active officer_appointments -> org_positions.slug.
SELECT DISTINCT m.user_id, u.email FROM organization_memberships m
JOIN users u ON u.id = m.user_id
WHERE m.organization_id = $1 AND m.lifecycle_state IN ('active','life_member')
  AND (sqlc.arg(all_members)::bool
       OR m.category = ANY(sqlc.arg(categories)::text[])
       OR m.org_unit_id = ANY(sqlc.arg(unit_ids)::uuid[])
       OR m.id IN (SELECT oa.member_id FROM officer_appointments oa
         JOIN org_positions p ON p.id = oa.position_id
         WHERE p.slug = ANY(sqlc.arg(positions)::text[])
           AND (oa.ends_at IS NULL OR oa.ends_at >= CURRENT_DATE)));

-- name: MeetingAudienceUserIDs :many
-- meeting_reminder fan-out (MeetingReminderDue): unit-scoped meetings
-- notify their home-unit + committee members; org-wide meetings notify
-- every active/life member.
SELECT DISTINCT m.user_id FROM organization_memberships m
WHERE m.organization_id = $1 AND m.lifecycle_state IN ('active','life_member')
  AND ($2::uuid IS NULL
       OR m.org_unit_id = $2
       OR m.id IN (SELECT cm.member_id FROM committee_memberships cm
                   WHERE cm.org_unit_id = $2));

-- name: PublishAnnouncement :one
-- draft → published CAS; published/retracted rows can't re-fire the fan-out.
UPDATE announcements SET state = 'published', published_at = now()
WHERE id = $1 AND organization_id = $2 AND state = 'draft'
RETURNING *;
