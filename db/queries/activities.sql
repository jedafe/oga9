-- Ref: oga9-engineering/sections/13-events-meetings.md — events & meetings.
-- Every query is org-scoped (Rule 24); attendances is a composite-PK
-- mark sheet (bulk upsert); minutes are versioned + immutable on approve.

-- name: CreateActivity :one
INSERT INTO activities (organization_id, kind, title, description, venue,
  starts_at, ends_at, unit_id, quorum_percent, capacity, created_by, recurrence_id)
VALUES (sqlc.arg(organization_id), sqlc.arg(kind), sqlc.arg(title),
  sqlc.arg(description), sqlc.arg(venue), sqlc.arg(starts_at), sqlc.arg(ends_at),
  sqlc.narg(unit_id)::uuid, sqlc.narg(quorum_percent), sqlc.narg(capacity),
  sqlc.arg(created_by), sqlc.narg(recurrence_id)::uuid)
RETURNING *;

-- name: GetActivity :one
SELECT * FROM activities WHERE id = $1 AND organization_id = $2;

-- name: ListActivities :many
SELECT * FROM activities WHERE organization_id = $1
  AND ($2::text = '' OR kind = $2)
  AND ($3::text = '' OR status = $3)
  AND ($4::uuid IS NULL OR unit_id = $4)
ORDER BY starts_at DESC LIMIT 200;

-- name: UpdateActivity :execrows
UPDATE activities SET
  title = COALESCE(sqlc.narg(title)::text, title),
  description = COALESCE(sqlc.narg(description)::text, description),
  venue = COALESCE(sqlc.narg(venue)::text, venue),
  starts_at = COALESCE(sqlc.narg(starts_at)::timestamptz, starts_at),
  ends_at = COALESCE(sqlc.narg(ends_at)::timestamptz, ends_at),
  unit_id = CASE WHEN sqlc.arg(clear_unit)::boolean THEN NULL
                 ELSE COALESCE(sqlc.narg(unit_id)::uuid, unit_id) END,
  quorum_percent = COALESCE(sqlc.narg(quorum_percent)::int, quorum_percent),
  capacity = COALESCE(sqlc.narg(capacity)::int, capacity),
  status = COALESCE(sqlc.narg(status)::text, status),
  -- a reschedule re-arms the reminder: a moved meeting gets a fresh notice.
  reminder_sent = reminder_sent AND sqlc.narg(starts_at) IS NULL
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(organization_id)::uuid
  AND status <> 'cancelled'; -- cancelled is terminal

-- name: CancelActivity :execrows
UPDATE activities SET status = 'cancelled'
WHERE id = $1 AND organization_id = $2 AND status <> 'cancelled';

-- name: OrgCalendar :many
-- forward-looking org calendar (meetings + events, unit filter).
SELECT * FROM activities WHERE organization_id = $1
  AND ($2::uuid IS NULL OR unit_id = $2 OR unit_id IS NULL) -- a unit's calendar includes org-wide activities
  AND status <> 'cancelled'
  AND starts_at >= now() - interval '1 day'
ORDER BY starts_at LIMIT 300;

-- name: MyCalendar :many
-- /me/calendar — activities across every org the user is a member of.
SELECT a.* FROM activities a
JOIN organization_memberships m ON m.organization_id = a.organization_id
WHERE m.user_id = $1
  AND m.lifecycle_state IN ('active','suspended','life_member')
  AND a.status <> 'cancelled' AND a.starts_at >= now() - interval '1 day'
ORDER BY a.starts_at LIMIT 300;

-- ---------- agendas / minutes ----------
-- name: GetAgenda :one
SELECT * FROM meeting_agendas WHERE activity_id = $1;

-- name: UpsertAgenda :one
INSERT INTO meeting_agendas (activity_id, items)
VALUES (sqlc.arg(activity_id), sqlc.arg(items))
ON CONFLICT (activity_id) DO UPDATE SET items = EXCLUDED.items
RETURNING *;

-- name: CreateMinutes :one
-- version = prior max + 1 (versioned per edit until approved).
INSERT INTO minutes (activity_id, body, version)
SELECT sqlc.arg(activity_id), sqlc.arg(body),
       COALESCE(max(version),0)+1 FROM minutes WHERE activity_id = sqlc.arg(activity_id)
GROUP BY activity_id
RETURNING *;

-- name: CreateMinutesFirst :one
-- First draft when no minutes row exists yet.
INSERT INTO minutes (activity_id, body, version)
VALUES (sqlc.arg(activity_id), sqlc.arg(body), 1)
RETURNING *;

-- name: ListMinutes :many
SELECT * FROM minutes WHERE activity_id = $1
  AND EXISTS (SELECT 1 FROM activities a WHERE a.id = minutes.activity_id AND a.organization_id = $2)
ORDER BY version DESC;

-- name: ApproveMinutes :one
-- draft → approved CAS; approved rows are immutable (no UPDATE surface).
UPDATE minutes SET state = 'approved', approved_by = $2, approved_at = now()
WHERE minutes.id = $1 AND minutes.state = 'draft'
  AND minutes.activity_id IN (SELECT a.id FROM activities a WHERE a.organization_id = $3)
RETURNING *;

-- name: ArchiveMinutes :execrows
-- approved → archived when a newer approved version exists.
UPDATE minutes SET state = 'archived'
WHERE activity_id = $1 AND state = 'approved' AND id <> $2;

-- ---------- action items ----------
-- name: CreateActionItem :one
INSERT INTO action_items (activity_id, assignee_id, title, due_date)
VALUES (sqlc.arg(activity_id), sqlc.arg(assignee_id), sqlc.arg(title), sqlc.narg(due_date))
RETURNING *;

-- name: ListActionItems :many
-- $4 scopes the list to one assignee — §13 view_own: non-manage callers
-- see only their own items.
SELECT * FROM action_items WHERE activity_id = $1
  AND EXISTS (SELECT 1 FROM activities a WHERE a.id = action_items.activity_id AND a.organization_id = $3)
  AND ($2::text = '' OR state = $2)
  AND ($4::uuid IS NULL OR assignee_id = $4) ORDER BY created_at LIMIT 200;

-- name: MyActionItems :many
-- /me/action-items — assigned to the user across orgs.
SELECT i.* FROM action_items i
JOIN organization_memberships m ON m.id = i.assignee_id
WHERE m.user_id = $1 AND i.state IN ('open','overdue') ORDER BY i.due_date NULLS LAST LIMIT 200;

-- name: CompleteActionItem :execrows
-- done_at stamped here; assignee or officer both reach this (perm gate is
-- in the route layer — view_own for self, org.action_items.manage else).
UPDATE action_items SET state = 'done', done_at = now()
WHERE action_items.id = $1 AND assignee_id = $2 AND state IN ('open','overdue')
  AND action_items.activity_id IN (SELECT a.id FROM activities a WHERE a.organization_id = $3);

-- name: CompleteActionItemManage :execrows
UPDATE action_items SET state = 'done', done_at = now()
WHERE action_items.id = $1 AND state IN ('open','overdue')
  AND action_items.activity_id IN (SELECT a.id FROM activities a WHERE a.organization_id = $2);

-- name: PatchActionItem :execrows
UPDATE action_items SET
  title = COALESCE(sqlc.narg(title)::text, title),
  due_date = COALESCE(sqlc.narg(due_date)::date, due_date),
  assignee_id = COALESCE(sqlc.narg(assignee_id)::uuid, assignee_id)
WHERE id = sqlc.arg(id)::uuid AND state IN ('open','overdue')
  AND action_items.activity_id IN (SELECT a.id FROM activities a WHERE a.organization_id = sqlc.arg(organization_id)::uuid);

-- name: MarkOverdueActionItems :many
-- daily sweep: open items past due_date flip to overdue.
UPDATE action_items SET state = 'overdue'
WHERE state = 'open' AND due_date IS NOT NULL AND due_date < now()::date
RETURNING id, activity_id, assignee_id;

-- ---------- attendance ----------
-- name: MarkAttendance :exec
-- bulk sheet upsert — one row per member per activity.
INSERT INTO attendances (activity_id, membership_id, status, checked_at, note)
VALUES (sqlc.arg(activity_id), sqlc.arg(membership_id), sqlc.arg(status),
  sqlc.narg(checked_at), sqlc.narg(note))
ON CONFLICT (activity_id, membership_id) DO UPDATE
  SET status = EXCLUDED.status, checked_at = EXCLUDED.checked_at, note = EXCLUDED.note;

-- name: ListAttendance :many
SELECT * FROM attendances WHERE activity_id = $1 ORDER BY membership_id;

-- name: AttendanceCount :one
-- quorum check: present+late count over the org's eligible membership.
SELECT count(*) FILTER (WHERE a.status IN ('present','late')) AS present
FROM attendances a WHERE a.activity_id = $1;

-- ---------- registrations / check-in ----------
-- name: RegisterEvent :one
INSERT INTO event_registrations (activity_id, membership_id, guest_name, guest_contact, registered_by)
VALUES (sqlc.arg(activity_id), sqlc.narg(membership_id), sqlc.narg(guest_name),
  sqlc.narg(guest_contact), sqlc.narg(registered_by))
ON CONFLICT (activity_id, membership_id) WHERE membership_id IS NOT NULL DO NOTHING
RETURNING *;

-- name: GuestCountByMember :one
-- Guest-cap enforcement: how many guests a member has already registered
-- for this event (org.events.guest_cap).
SELECT count(*) FROM event_registrations
WHERE activity_id = $1 AND registered_by = $2 AND membership_id IS NULL
  AND status <> 'cancelled';

-- name: EventHeadcount :one
-- capacity check counts live registrations (registered + checked_in).
SELECT count(*) FROM event_registrations
WHERE activity_id = $1 AND status IN ('registered','checked_in');

-- name: ListRegistrations :many
SELECT * FROM event_registrations WHERE activity_id = $1 ORDER BY created_at;

-- name: CheckIn :one
-- registered → checked_in CAS; also usable for re-marking no_show back.
UPDATE event_registrations SET status = 'checked_in'
WHERE activity_id = $1 AND membership_id = $2 AND status IN ('registered','no_show')
  AND EXISTS (SELECT 1 FROM activities a WHERE a.id = event_registrations.activity_id AND a.organization_id = $3)
RETURNING *;

-- name: CheckInByID :one
-- Guest check-in addresses the registration row directly (guests carry no
-- membership_id) — still org-scoped through the parent activity.
UPDATE event_registrations SET status = 'checked_in'
WHERE event_registrations.id = $1 AND activity_id = $2 AND status IN ('registered','no_show')
  AND EXISTS (SELECT 1 FROM activities a WHERE a.id = event_registrations.activity_id AND a.organization_id = $3)
RETURNING *;

-- ---------- recurrences ----------
-- name: ActiveRecurrences :many
SELECT * FROM activity_recurrences WHERE active;

-- name: CreateRecurrence :one
INSERT INTO activity_recurrences (organization_id, kind, template, rrule)
VALUES (sqlc.arg(organization_id), sqlc.arg(kind), sqlc.arg(template), sqlc.arg(rrule))
RETURNING *;

-- name: ListRecurrences :many
SELECT * FROM activity_recurrences WHERE organization_id = $1 AND active ORDER BY id;

-- name: CountRecurrenceInstances :one
-- COUNT semantics cap TOTAL occurrences per series, not per window —
-- the sweep subtracts what's already generated.
SELECT count(*) FROM activities WHERE recurrence_id = $1;


-- name: GetActivityAny :one
-- Task/sweep context — org read from the row.
SELECT * FROM activities WHERE id = $1;

-- name: CountOrgMembers :one
-- quorum denominator: active-equivalent members of the org.
SELECT count(*) FROM organization_memberships
WHERE organization_id = $1 AND lifecycle_state IN ('active','life_member');

-- name: GetOrgUnit :one
SELECT * FROM org_units WHERE id = $1 AND organization_id = $2;

-- name: GetActivityForUpdate :one
-- Serialize the register path per event: FOR UPDATE stops two concurrent
-- registrations both passing the capacity headcount.
SELECT * FROM activities WHERE id = $1 FOR UPDATE;

-- name: ClaimReminders :many
-- emit-once: meetings inside their org's notice window claim and return.
-- Window = org.meeting.notice_days (default 7); a fixed window would
-- shorten every longer-notice org's reminder.
UPDATE activities SET reminder_sent = true
WHERE status = 'scheduled' AND kind = 'meeting' AND NOT reminder_sent
  AND starts_at > now()
  AND starts_at <= now() + make_interval(days =>
    COALESCE((SELECT (os.value#>>'{}')::int FROM org_settings os
      WHERE os.organization_id = activities.organization_id
        AND os.key = 'org.meeting.notice_days'), 7))
RETURNING *;

-- name: CountUnitMembers :one
-- Quorum denominator: NULL unit = the whole org's roster; a unit = its
-- committee roster (committee_memberships.member_id) plus members whose
-- home unit it is (organization_memberships.org_unit_id) — branches have
-- no committee rows.
SELECT count(DISTINCT m.id) FROM organization_memberships m
WHERE m.organization_id = $1
  AND m.lifecycle_state IN ('active','life_member')
  AND ($2::uuid IS NULL
       OR m.org_unit_id = $2
       OR m.id IN (SELECT cm.member_id FROM committee_memberships cm
                   WHERE cm.org_unit_id = $2));

-- name: CreateRecurrenceInstance :one
-- atomic dedupe via activities_recurrence_instance: a duplicate
-- (recurrence_id, starts_at) returns no row instead of erroring.
INSERT INTO activities (organization_id, kind, title, description, venue,
  starts_at, ends_at, unit_id, quorum_percent, capacity, created_by, recurrence_id)
VALUES (sqlc.arg(organization_id), sqlc.arg(kind), sqlc.arg(title),
  sqlc.arg(description), sqlc.arg(venue), sqlc.arg(starts_at), sqlc.arg(ends_at),
  sqlc.narg(unit_id)::uuid, sqlc.narg(quorum_percent), sqlc.narg(capacity),
  sqlc.arg(created_by), sqlc.arg(recurrence_id))
ON CONFLICT (recurrence_id, starts_at) WHERE recurrence_id IS NOT NULL DO NOTHING
RETURNING *;

-- name: CancelActionItem :execrows
UPDATE action_items SET state = 'cancelled'
WHERE action_items.id = $1 AND state IN ('open','overdue')
  AND action_items.activity_id IN (SELECT a.id FROM activities a WHERE a.organization_id = $2);

-- name: GetMinute :one
SELECT * FROM minutes WHERE id = $1;

-- name: OpenActionItemsForOrg :many
-- §16 handover section: every still-open/overdue item across the org's activities.
SELECT i.* FROM action_items i
JOIN activities a ON a.id = i.activity_id
WHERE a.organization_id = $1 AND i.state IN ('open','overdue')
ORDER BY i.due_date NULLS LAST;
