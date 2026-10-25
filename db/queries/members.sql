-- Ref: oga9-engineering/sections/11-members.md (Phase 08)
-- Members: applications, ADR-014 lifecycle transitions, member_no sequence,
-- sponsors, renewals, cards, imports.

-- name: CreateApplication :one
INSERT INTO membership_applications (organization_id, user_id, email, phone, answers, sponsor_member_id)
VALUES (sqlc.arg(organization_id), sqlc.arg(user_id), sqlc.arg(email), sqlc.arg(phone),
        sqlc.arg(answers)::jsonb, sqlc.arg(sponsor_member_id))
RETURNING id;

-- name: GetApplication :one
SELECT id, organization_id, user_id, email, phone, answers, sponsor_member_id, status, created_at
FROM membership_applications WHERE id = $1;

-- name: ListApplications :many
SELECT id, user_id, email, phone, status, sponsor_member_id, answers, created_at, decided_at
FROM membership_applications
WHERE organization_id = sqlc.arg(org_id)::uuid AND status = sqlc.arg(status)
ORDER BY created_at;

-- name: SetApplicationStatus :one
-- Decision flow only moves forward within the intake machine
-- (applied→screened→approved|rejected|withdrawn); callers pass expected
-- source states so a concurrent decision can't double-apply.
UPDATE membership_applications SET status = $3, screened_by = $4, decided_at = now()
WHERE id = $1 AND status = ANY($2::text[])
RETURNING id, organization_id, sponsor_member_id;

-- name: NextMemberNoSeq :one
-- Atomic per-org sequence for member_no ({SLUG}-{YYYY}-{seq5}); the lock
-- serializes concurrent approvals so numbers are unique and never reused.
UPDATE organizations SET member_no_seq = member_no_seq + 1
WHERE id = $1
RETURNING member_no_seq;

-- name: GetOrgSlug :one
SELECT slug FROM organizations WHERE id = $1;

-- name: SetMemberNo :exec
UPDATE organization_memberships SET member_no = $2 WHERE id = $1;

-- name: GetMembershipFull :one
SELECT m.id, m.organization_id, m.user_id, m.member_no, m.category, m.lifecycle_state,
       m.dues_standing, m.last_renewed_at, m.joined_at, m.created_at,
       u.display_name, u.email
FROM organization_memberships m
JOIN users u ON u.id = m.user_id
WHERE m.id = $1;

-- name: ListMembers :many
SELECT m.id, m.user_id, m.member_no, m.category, m.lifecycle_state, m.dues_standing,
       m.joined_at, u.display_name, u.email
FROM organization_memberships m
JOIN users u ON u.id = m.user_id
WHERE m.organization_id = sqlc.arg(org_id)::uuid
  AND (sqlc.arg(category) = '' OR m.category = sqlc.arg(category))
  AND (sqlc.arg(state) = '' OR m.lifecycle_state = sqlc.arg(state))
ORDER BY u.display_name;

-- name: TransitionMembership :one
-- Guarded lifecycle write: fires only while the membership still holds an
-- expected source state — a stale read can't regress or double-transition.
-- joined_at stamps on first activation (invite path already stamps on
-- CreateMembership; the application pipeline lands it here).
UPDATE organization_memberships SET lifecycle_state = $3,
  joined_at = CASE WHEN $3 = 'active' AND joined_at IS NULL THEN now() ELSE joined_at END
WHERE id = $1 AND organization_id = $2 AND lifecycle_state = ANY($4::text[])
RETURNING id, user_id;

-- name: PatchMembership :one
-- Limited self/admin-editable fields; lifecycle never changes here.
UPDATE organization_memberships SET category = COALESCE(sqlc.narg(category), category)
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(org_id)::uuid
RETURNING id;

-- name: AddSponsor :exec
INSERT INTO member_sponsors (membership_id, sponsor_member_id, kind)
VALUES (sqlc.arg(membership_id), sqlc.arg(sponsor_member_id), sqlc.arg(kind))
ON CONFLICT (membership_id, sponsor_member_id) DO NOTHING;

-- name: CountSponsors :one
SELECT count(*) FROM member_sponsors WHERE membership_id = $1 AND kind = 'sponsor';

-- name: ListSponsors :many
SELECT ms.sponsor_member_id, ms.kind, u.display_name
FROM member_sponsors ms
JOIN organization_memberships m ON m.id = ms.sponsor_member_id
JOIN users u ON u.id = m.user_id
WHERE ms.membership_id = sqlc.arg(membership_id)::uuid;

-- name: CreateRenewal :one
INSERT INTO membership_renewals (membership_id, period_label, starts_at, ends_at)
VALUES (sqlc.arg(membership_id), sqlc.arg(period_label), sqlc.arg(starts_at), sqlc.arg(ends_at))
RETURNING id;

-- name: TouchRenewal :exec
UPDATE organization_memberships SET last_renewed_at = now() WHERE id = $1;

-- name: ListRenewals :many
SELECT id, period_label, starts_at, ends_at, created_at
FROM membership_renewals WHERE membership_id = sqlc.arg(membership_id)::uuid
ORDER BY starts_at DESC;

-- name: IssueCard :one
INSERT INTO membership_cards (membership_id, serial, design)
VALUES (sqlc.arg(membership_id), sqlc.arg(serial), sqlc.arg(design)::jsonb)
RETURNING id, serial, issued_at;

-- name: RevokeCards :exec
-- Terminal lifecycle events (expelled/deceased) pull the physical card —
-- Rule: MemberDeceased → card revoke (§11 business rules).
UPDATE membership_cards SET revoked_at = now()
WHERE membership_id = $1 AND revoked_at IS NULL;

-- name: GetActiveCard :one
SELECT id, serial, issued_at FROM membership_cards
WHERE membership_id = $1 AND revoked_at IS NULL;

-- name: CreateImport :one
INSERT INTO member_imports (organization_id, uploaded_by, filename, data)
VALUES (sqlc.arg(organization_id), sqlc.arg(uploaded_by), sqlc.arg(filename), sqlc.arg(data)::jsonb)
RETURNING id;

-- name: GetImport :one
SELECT id, organization_id, uploaded_by, filename, data, status, errors
FROM member_imports WHERE id = $1;

-- name: FinishImport :exec
UPDATE member_imports SET status = $2, row_count = $3, ok_count = $4, err_count = $5, errors = $6
WHERE id = $1;

-- name: EndMemberAppointments :many
-- Terminal transitions vacate the member's open office seats (§11:
-- deceased/expelled → position vacancies). Same-tx + AppointmentEnded.
UPDATE officer_appointments SET ends_at = GREATEST(starts_at, CURRENT_DATE)
WHERE member_id = $1 AND ends_at IS NULL
RETURNING id, term_id;

-- name: RemoveMemberCommitteeSeats :many
DELETE FROM committee_memberships
WHERE member_id = $1
RETURNING org_unit_id;

-- name: MemberHistory :many
-- Lifecycle journey: sourced domain events against the membership/user
-- aggregates (ADR-014 "complete replayable journey").
SELECT event_type, payload, occurred_at FROM domain_events
WHERE organization_id = sqlc.arg(org_id)::uuid
  AND (aggregate_id = sqlc.arg(membership_id)::uuid
       OR (aggregate_type = 'user' AND aggregate_id = sqlc.arg(user_id)::uuid))
ORDER BY occurred_at;

-- name: GetApplicationCountForUser :one
SELECT count(*) FROM membership_applications
WHERE organization_id = $1 AND user_id = $2 AND status IN ('applied','screened','approved');

-- name: ListUserMemberships :many
SELECT m.id, m.organization_id, o.name AS org_name, o.slug AS org_slug,
       m.member_no, m.category, m.lifecycle_state, m.dues_standing, m.joined_at
FROM organization_memberships m
JOIN organizations o ON o.id = m.organization_id AND o.deleted_at IS NULL
WHERE m.user_id = $1
ORDER BY o.name;

-- name: GetPublicOrgBySlug :one
SELECT id, name FROM organizations
WHERE slug = $1 AND is_public_listed AND deleted_at IS NULL;
