-- Ref: oga9-engineering/sections/17-welfare.md

-- name: CreateWelfareCase :one
INSERT INTO welfare_cases (organization_id, membership_id, kind, title, details,
  requested_amount_minor, currency, auto_suggested)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(membership_id)::uuid, sqlc.arg(kind),
  sqlc.arg(title), sqlc.narg(details), sqlc.narg(requested_amount_minor),
  sqlc.narg(currency), sqlc.arg(auto_suggested))
RETURNING id;

-- name: GetWelfareCase :one
SELECT * FROM welfare_cases
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(org_id)::uuid;

-- name: ListWelfareCases :many
SELECT * FROM welfare_cases
WHERE organization_id = sqlc.arg(org_id)::uuid
  AND (sqlc.narg(state)::text = '' OR state = sqlc.narg(state)::text)
ORDER BY created_at DESC LIMIT 200;

-- name: MyWelfareCases :many
-- /me/welfare — the member's own cases across orgs they belong to.
SELECT wc.* FROM welfare_cases wc
JOIN organization_memberships m ON m.id = wc.membership_id
WHERE m.user_id = sqlc.arg(user_id)::uuid
ORDER BY wc.created_at DESC LIMIT 100;

-- name: PriorCaseInWindow :one
-- org.welfare.once_per.{kind}_days — a repeat request inside the window is
-- flagged (not auto-rejected; the flag rides on the case for reviewers).
SELECT id FROM welfare_cases
WHERE membership_id = sqlc.arg(membership_id)::uuid AND kind = sqlc.arg(kind)
  AND created_at > now() - (sqlc.arg(days)::int * interval '1 day')
  AND state <> 'rejected'
ORDER BY created_at DESC LIMIT 1;

-- name: SetWelfareCaseState :execrows
-- CAS on the expected state — concurrent reviewers can't double-decide.
UPDATE welfare_cases SET state = sqlc.arg(to_state)
WHERE id = sqlc.arg(id)::uuid AND state = sqlc.arg(from_state);

-- name: AddCaseEvent :one
-- append-only case history — visible to member + welfare officers.
INSERT INTO welfare_case_events (case_id, actor_id, action, note)
VALUES (sqlc.arg(case_id)::uuid, sqlc.narg(actor_id)::uuid, sqlc.arg(action), sqlc.narg(note))
RETURNING id;

-- name: ListCaseEvents :many
SELECT * FROM welfare_case_events
WHERE case_id = sqlc.arg(case_id)::uuid ORDER BY created_at;

-- name: AddApproval :one
INSERT INTO welfare_approvals (case_id, approver_id, decision,
  approved_amount_minor, currency)
VALUES (sqlc.arg(case_id)::uuid, sqlc.arg(approver_id)::uuid, sqlc.arg(decision),
  sqlc.narg(approved_amount_minor), sqlc.narg(currency))
RETURNING id;

-- name: CountCaseApprovals :one
SELECT count(*) FROM welfare_approvals
WHERE case_id = $1 AND decision = 'approved';

-- name: ApproverAlreadyDecided :one
-- one approver, one decision per case.
SELECT EXISTS(SELECT 1 FROM welfare_approvals
  WHERE case_id = $1 AND approver_id = $2);

-- name: ListApprovals :many
SELECT * FROM welfare_approvals WHERE case_id = $1 ORDER BY decided_at;

-- name: CreateVisit :one
INSERT INTO welfare_visits (case_id, scheduled_at)
VALUES (sqlc.arg(case_id)::uuid, sqlc.narg(scheduled_at))
RETURNING id;

-- name: AddVisitDelegate :exec
INSERT INTO welfare_visit_delegates (visit_id, member_id)
VALUES (sqlc.arg(visit_id)::uuid, sqlc.arg(member_id)::uuid)
ON CONFLICT DO NOTHING;

-- name: CompleteVisit :execrows
UPDATE welfare_visits SET completed_at = now(), report = $2
WHERE id = $1 AND completed_at IS NULL;

-- name: ListVisits :many
SELECT * FROM welfare_visits WHERE case_id = $1 ORDER BY scheduled_at;

-- name: AddBenefit :one
INSERT INTO welfare_benefits (case_id, kind, amount_minor, currency, disbursed_at)
VALUES (sqlc.arg(case_id)::uuid, sqlc.arg(kind), sqlc.narg(amount_minor),
  sqlc.narg(currency), now())
RETURNING id;

-- name: ListBenefits :many
SELECT * FROM welfare_benefits WHERE case_id = $1 ORDER BY disbursed_at;

-- name: WelfareSettings :many
-- the org.welfare.* org_settings namespace — the policies endpoint (Rule 23).
SELECT key, value FROM org_settings
WHERE organization_id = sqlc.arg(org_id)::uuid
  AND key LIKE 'org.welfare.%'
ORDER BY key;

-- name: CaseCountByKind :many
SELECT kind, count(*)::bigint AS n FROM welfare_cases
WHERE organization_id = sqlc.arg(org_id)::uuid
GROUP BY kind ORDER BY n DESC;
