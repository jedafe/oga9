-- Ref: oga9-engineering/sections/27-volunteers.md — volunteer roles +
-- signups. Signups are audited, never deleted; standby queue promotes
-- first-come when a confirmed slot frees.

-- ---------- roles ----------
-- name: CreateVolunteerRole :one
INSERT INTO volunteer_roles (organization_id, activity_id, unit_id, name,
  slots, note, created_by)
VALUES (sqlc.arg(organization_id), sqlc.arg(activity_id),
  sqlc.narg(unit_id)::uuid, sqlc.arg(name), sqlc.arg(slots),
  sqlc.narg(note)::text, sqlc.arg(created_by))
RETURNING *;

-- name: GetVolunteerRole :one
SELECT * FROM volunteer_roles WHERE id = $1 AND organization_id = $2;

-- name: ListVolunteerRoles :many
SELECT * FROM volunteer_roles
WHERE activity_id = $1 AND organization_id = $2
ORDER BY created_at;

-- name: UpdateVolunteerRole :execrows
UPDATE volunteer_roles SET
  name = COALESCE(sqlc.narg(name)::text, name),
  slots = COALESCE(sqlc.narg(slots)::int, slots),
  note = COALESCE(sqlc.narg(note)::text, note),
  unit_id = CASE WHEN sqlc.arg(clear_unit)::boolean THEN NULL
                 ELSE COALESCE(sqlc.narg(unit_id)::uuid, unit_id) END
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(organization_id)::uuid;

-- name: DeleteVolunteerRole :execrows
-- roles delete only while no confirmed/completed signups exist (§27).
DELETE FROM volunteer_roles WHERE volunteer_roles.id = $1 AND organization_id = $2
  AND NOT EXISTS (SELECT 1 FROM volunteer_signups s
    WHERE s.role_id = volunteer_roles.id
      AND s.state IN ('confirmed','completed'));

-- name: ConfirmedCount :one
-- capacity check inside the confirm tx (role row must be locked first).
SELECT count(*)::int FROM volunteer_signups
WHERE role_id = $1 AND state IN ('confirmed','completed');

-- name: LockVolunteerRole :one
SELECT id, slots FROM volunteer_roles WHERE id = $1 AND organization_id = $2
FOR UPDATE;

-- ---------- signups ----------
-- name: SignupVolunteer :one
INSERT INTO volunteer_signups (role_id, membership_id, note)
VALUES ($1, $2, $3)
ON CONFLICT (role_id, membership_id) DO NOTHING
RETURNING *;

-- name: GetSignup :one
SELECT s.* FROM volunteer_signups s
JOIN volunteer_roles r ON r.id = s.role_id
WHERE s.id = $1 AND r.organization_id = $2;

-- name: ConfirmSignup :one
UPDATE volunteer_signups SET state = 'confirmed', decided_by = $3
WHERE volunteer_signups.id = $1 AND state = 'signed_up'
  AND role_id IN (SELECT volunteer_roles.id FROM volunteer_roles WHERE organization_id = $2)
RETURNING *;

-- name: MarkSignup :one
-- officer marks confirmed signups completed|no_show after the activity.
UPDATE volunteer_signups SET state = $3, decided_by = $4,
  completed_at = CASE WHEN $3 = 'completed' THEN now() ELSE completed_at END
WHERE volunteer_signups.id = $1 AND state = 'confirmed'
  AND role_id IN (SELECT volunteer_roles.id FROM volunteer_roles WHERE organization_id = $2)
RETURNING *;

-- name: CancelSignup :one
UPDATE volunteer_signups SET state = 'cancelled', decided_by = $3
WHERE volunteer_signups.id = $1 AND state IN ('signed_up','confirmed')
  AND role_id IN (SELECT volunteer_roles.id FROM volunteer_roles WHERE organization_id = $2)
RETURNING *;

-- name: CancelSignupSelf :one
-- self-withdraw — the caller's own signup only.
UPDATE volunteer_signups SET state = 'cancelled'
WHERE id = $1 AND membership_id = $2 AND state IN ('signed_up','confirmed')
RETURNING *;

-- name: OldestStandby :one
-- standby promotion: first-come signed_up row promotes when a slot frees.
SELECT * FROM volunteer_signups
WHERE role_id = $1 AND state = 'signed_up'
ORDER BY signed_up_at LIMIT 1;

-- name: RoleRoster :many
SELECT s.id, s.role_id, s.membership_id, s.state, s.note, s.signed_up_at,
  s.completed_at, m.member_no, u.display_name
FROM volunteer_signups s
JOIN organization_memberships m ON m.id = s.membership_id
JOIN users u ON u.id = m.user_id
WHERE s.role_id = $1 ORDER BY s.signed_up_at;

-- name: ActivityRoster :many
-- all signups across an activity's roles (roster view).
SELECT s.id, s.role_id, r.name AS role_name, s.membership_id, s.state,
  s.signed_up_at, s.completed_at, m.member_no, u.display_name
FROM volunteer_signups s
JOIN volunteer_roles r ON r.id = s.role_id
JOIN organization_memberships m ON m.id = s.membership_id
JOIN users u ON u.id = m.user_id
WHERE r.activity_id = $1 AND r.organization_id = $2
ORDER BY r.name, s.signed_up_at;

-- name: MemberInUnitTree :one
-- §27 eligibility: active members of the unit or its descendants (§10 tree).
WITH RECURSIVE sub AS (
  SELECT org_units.id FROM org_units WHERE org_units.id = $1 AND org_units.organization_id = $2
  UNION ALL
  SELECT u.id FROM org_units u JOIN sub s ON u.parent_unit_id = s.id
)
SELECT EXISTS(SELECT 1 FROM organization_memberships m
  JOIN sub ON m.org_unit_id = sub.id
  WHERE m.id = $3 AND m.lifecycle_state IN ('active','life_member'));

-- name: ConfirmedVolunteersDue :many
-- volunteer_remind sweep: confirmed signups whose activity starts within
-- the org's remind_hours window (org_setting wins over the arg default).
SELECT s.id AS signup_id, s.membership_id, m.user_id, r.name AS role_name,
  r.organization_id, a.id AS activity_id, a.title AS activity_title, a.starts_at
FROM volunteer_signups s
JOIN volunteer_roles r ON r.id = s.role_id
JOIN organization_memberships m ON m.id = s.membership_id
JOIN activities a ON a.id = r.activity_id
WHERE s.state = 'confirmed' AND a.status = 'scheduled'
  AND a.starts_at > now()
  AND a.starts_at <= now() + make_interval(hours => COALESCE((
      SELECT (o.value #>> '{}')::int FROM org_settings o
      WHERE o.organization_id = r.organization_id
        AND o.key = 'org.volunteers.remind_hours'
        AND jsonb_typeof(o.value) = 'number'), $1::int));

-- name: CancelSignupsForActivity :many
-- ActivityCancelled consumer — open signups on the activity's roles cancel.
UPDATE volunteer_signups SET state = 'cancelled'
WHERE state IN ('signed_up','confirmed')
  AND role_id IN (SELECT id FROM volunteer_roles WHERE activity_id = $1)
RETURNING id, role_id, membership_id;

-- name: MyVolunteerSignups :many
-- /me/volunteer-signups — the member's signups across orgs.
SELECT s.id, s.state, s.signed_up_at, r.name AS role_name,
  r.organization_id, a.id AS activity_id, a.title AS activity_title, a.starts_at
FROM volunteer_signups s
JOIN volunteer_roles r ON r.id = s.role_id
JOIN organization_memberships m ON m.id = s.membership_id
JOIN activities a ON a.id = r.activity_id
WHERE m.user_id = $1 ORDER BY a.starts_at DESC LIMIT 200;
