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
UPDATE membership_applications SET status = $3, screened_by = $4,
  decided_at = CASE WHEN $3 IN ('approved','rejected','withdrawn') THEN now() ELSE decided_at END
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
       m.dues_standing, m.last_renewed_at, m.joined_at, m.created_at, m.org_unit_id,
       u.display_name, u.email
FROM organization_memberships m
JOIN users u ON u.id = m.user_id
WHERE m.id = $1;

-- name: ListMembers :many
SELECT m.id, m.user_id, m.member_no, m.category, m.lifecycle_state, m.dues_standing,
       m.joined_at, m.org_unit_id, u.display_name, u.email
FROM organization_memberships m
JOIN users u ON u.id = m.user_id
WHERE m.organization_id = sqlc.arg(org_id)::uuid
  AND (sqlc.arg(category) = '' OR m.category = sqlc.arg(category))
  AND (sqlc.arg(state) = '' OR m.lifecycle_state = sqlc.arg(state))
  AND (sqlc.narg(unit_id)::uuid IS NULL OR m.org_unit_id = sqlc.narg(unit_id)::uuid)
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
UPDATE organization_memberships SET category = COALESCE(sqlc.narg(category), category),
  org_unit_id = COALESCE(sqlc.narg(org_unit_id)::uuid, org_unit_id)
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(org_id)::uuid
RETURNING id;

-- name: AddSponsor :exec
INSERT INTO member_sponsors (membership_id, sponsor_member_id, kind)
VALUES (sqlc.arg(membership_id), sqlc.arg(sponsor_member_id), sqlc.arg(kind))
ON CONFLICT DO NOTHING;

-- name: AddApplicationSponsor :exec
-- Extra sponsors attached at intake (sponsor_required up to 2); they
-- migrate onto the membership on approve.
INSERT INTO member_sponsors (application_id, sponsor_member_id, kind)
VALUES (sqlc.arg(application_id), sqlc.arg(sponsor_member_id), sqlc.arg(kind))
ON CONFLICT DO NOTHING;

-- name: CountApplicationSponsors :one
-- Total sponsors across the application's primary sponsor_member_id plus
-- any application-staged rows — the sponsor_required numerator at approve.
SELECT (
  SELECT count(*) FROM member_sponsors
  WHERE application_id = sqlc.arg(application_id)::uuid AND kind = 'sponsor'
) + (SELECT CASE WHEN sponsor_member_id IS NOT NULL THEN 1 ELSE 0 END
     FROM membership_applications WHERE id = sqlc.arg(application_id)::uuid);

-- name: CopyApplicationSponsors :exec
-- Staged sponsors become membership rows on approve; ON CONFLICT keeps a
-- staged sponsor that duplicates the primary from breaking the approve tx.
INSERT INTO member_sponsors (membership_id, sponsor_member_id, kind)
SELECT sqlc.arg(membership_id)::uuid, sponsor_member_id, kind
FROM member_sponsors WHERE application_id = sqlc.arg(application_id)::uuid
ON CONFLICT DO NOTHING;

-- name: DeleteApplicationSponsors :exec
DELETE FROM member_sponsors WHERE application_id = sqlc.arg(application_id)::uuid;

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
-- A recorded renewal settles the period — restore good standing (§12
-- payment consumers take over standing changes once finance lands).
UPDATE organization_memberships SET last_renewed_at = now(), dues_standing = 'good' WHERE id = $1;

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
-- RETURNING ids feed CommitteeMemberRemoved events — the seats can't be
-- deleted silently (Rule 12: state change + event in the same tx).
DELETE FROM committee_memberships
WHERE member_id = $1
RETURNING id, org_unit_id;

-- name: MemberHistory :many
-- Lifecycle journey: the SOURCED event stream over membership-family
-- aggregates only (the membership_history view contract, §11) — the
-- membership itself, its intake application, and its cards/renewals.
-- Identity events on the user aggregate stay out of this surface.
SELECT event_type, payload, occurred_at FROM domain_events
WHERE organization_id = sqlc.arg(org_id)::uuid
  AND sourced
  AND (
    (aggregate_type = 'organization_membership' AND aggregate_id = sqlc.arg(membership_id)::uuid)
    OR (aggregate_type = 'membership_application' AND aggregate_id IN (
          SELECT id FROM membership_applications
          WHERE organization_id = sqlc.arg(org_id)::uuid AND user_id = sqlc.arg(user_id)::uuid))
    OR (aggregate_type = 'membership_card' AND aggregate_id IN (
          SELECT id FROM membership_cards WHERE membership_id = sqlc.arg(membership_id)::uuid))
    OR (aggregate_type = 'membership_renewal' AND aggregate_id IN (
          SELECT id FROM membership_renewals WHERE membership_id = sqlc.arg(membership_id)::uuid)))
ORDER BY occurred_at;

-- name: WithdrawApplication :one
-- Self-service withdrawal: only the applicant's own row, and only while
-- pre-decision. CAS source states keep a raced admin decision from
-- being clobbered.
UPDATE membership_applications SET status = 'withdrawn', decided_at = now()
WHERE id = $1 AND user_id = $2 AND status IN ('applied','screened')
RETURNING id, organization_id;

-- name: GetApplicationCountForUser :one
-- Only UNDECIDED applications block a new one — a decided (approved/
-- rejected/withdrawn) row is history, not an open intake.
SELECT count(*) FROM membership_applications
WHERE organization_id = $1 AND user_id = $2 AND status IN ('applied','screened');

-- name: ExistsOpenApplicationByEmail :one
-- Import idempotency: a retried member_import worker must not mint a
-- second open application for the same applicant (Rule 27) — dedupe on
-- email when present, phone otherwise. Decided rows don't block.
SELECT EXISTS(
  SELECT 1 FROM membership_applications
  WHERE organization_id = sqlc.arg(org_id)::uuid AND status IN ('applied','screened')
    AND ((sqlc.arg(email) <> '' AND lower(email::text) = lower(sqlc.arg(email)))
      OR (sqlc.arg(email) = '' AND sqlc.arg(phone) <> '' AND phone = sqlc.arg(phone)))
);

-- name: ExistsOrgMemberByContact :one
-- Public-apply dedupe: an email that already belongs to a member of this
-- org must not mint a parallel application (the member applies through
-- their account, or not at all).
SELECT EXISTS(
  SELECT 1 FROM organization_memberships m
  JOIN users u ON u.id = m.user_id
  WHERE m.organization_id = sqlc.arg(org_id)::uuid
    AND sqlc.arg(email) <> '' AND lower(u.email) = lower(sqlc.arg(email))
);

-- name: ApprovedApplicationsForEmail :many
-- Registration hook (§11): pre-signup applications approved while the
-- applicant had no account materialize into memberships once their
-- verified email binds a user. user_id IS NULL keeps it idempotent —
-- linking the row consumes it.
SELECT id, organization_id, sponsor_member_id FROM membership_applications
WHERE user_id IS NULL AND status = 'approved' AND email = sqlc.arg(email);

-- name: LinkApplicationUser :execrows
-- Binds the applicant's new account — CAS on NULL so a raced bind can't
-- steal the row; 0 rows = already linked, treat as consumed.
UPDATE membership_applications SET user_id = $2
WHERE id = $1 AND user_id IS NULL;

-- name: ListStaleImports :many
-- Durable-row floor: imports recorded but never picked up (the enqueue
-- died) or whose worker was archived mid-run — re-driven by the
-- scheduler:tick sweep, mirroring ListStaleWebhookEvents (Rule 9-style).
SELECT id FROM member_imports
WHERE status = 'processing'
  AND created_at < now() - interval '30 minutes'
  AND created_at > now() - interval '7 days'
ORDER BY created_at LIMIT 100;

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

-- name: CreateExport :one
INSERT INTO member_exports (organization_id, requested_by, filename)
VALUES (sqlc.arg(organization_id), sqlc.arg(requested_by), sqlc.arg(filename))
RETURNING id;

-- name: FinishExport :exec
UPDATE member_exports SET status = $2, content = $3, row_count = $4 WHERE id = $1;

-- name: GetExport :one
SELECT * FROM member_exports WHERE id = $1 AND organization_id = $2;

-- name: GetExportAny :one
SELECT * FROM member_exports WHERE id = $1;

-- name: ListMembersForExport :many
SELECT m.member_no, u.display_name, u.email, m.category, m.lifecycle_state,
       m.dues_standing, m.joined_at
FROM organization_memberships m
JOIN users u ON u.id = m.user_id
WHERE m.organization_id = $1 ORDER BY m.member_no;
