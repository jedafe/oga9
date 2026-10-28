-- Ref: oga9-engineering/sections/16-governance.md

-- name: CreateMotion :one
INSERT INTO motions (organization_id, activity_id, mover_id, seconder_id, title, body)
VALUES (sqlc.arg(org_id)::uuid, sqlc.narg(activity_id)::uuid,
        sqlc.arg(mover_id)::uuid, sqlc.narg(seconder_id)::uuid,
        sqlc.arg(title), sqlc.arg(body))
RETURNING id;

-- name: ListMotions :many
SELECT * FROM motions
WHERE organization_id = sqlc.arg(org_id)::uuid
  AND (sqlc.narg(state)::text = '' OR state = sqlc.narg(state)::text)
ORDER BY created_at DESC LIMIT 200;

-- name: GetMotion :one
SELECT * FROM motions
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(org_id)::uuid;

-- name: SetMotionState :execrows
-- state machine is one-way: tabled → debated → voted | withdrawn.
-- CAS on the read state so concurrent recorders can't double-vote.
UPDATE motions SET state = sqlc.arg(to_state)
WHERE id = sqlc.arg(id)::uuid AND state = sqlc.arg(from_state);

-- name: CreateResolution :one
-- term-scoped (Rule 21): institutional record survives handover.
INSERT INTO resolutions (organization_id, motion_id, activity_id, term_id,
  title, body, outcome, votes_for, votes_against, votes_abstain)
VALUES (sqlc.arg(org_id)::uuid, sqlc.narg(motion_id)::uuid, sqlc.narg(activity_id)::uuid,
  sqlc.arg(term_id)::uuid, sqlc.arg(title), sqlc.arg(body), sqlc.arg(outcome),
  sqlc.narg(votes_for), sqlc.narg(votes_against), sqlc.narg(votes_abstain))
RETURNING id;

-- name: ListResolutions :many
SELECT * FROM resolutions
WHERE organization_id = sqlc.arg(org_id)::uuid
ORDER BY created_at DESC LIMIT 200;

-- name: GetResolution :one
SELECT * FROM resolutions
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(org_id)::uuid;

-- name: CurrentTermID :one
-- resolution/motion records bind to the current term.
SELECT id FROM terms
WHERE organization_id = sqlc.arg(org_id)::uuid AND is_current;

-- name: AdoptConstitution :one
-- version is max+1 per org — v1 stays readable (append-only version log).
INSERT INTO constitution_versions (organization_id, version, body, adopted_by_resolution_id)
VALUES (sqlc.arg(org_id)::uuid,
        (SELECT COALESCE(MAX(version),0)+1 FROM constitution_versions
         WHERE organization_id = sqlc.arg(org_id)::uuid),
        sqlc.arg(body), sqlc.narg(resolution_id)::uuid)
RETURNING id, version;

-- name: ListConstitution :many
SELECT * FROM constitution_versions
WHERE organization_id = sqlc.arg(org_id)::uuid
ORDER BY version DESC LIMIT 50;

-- name: CurrentConstitution :one
-- current = max adopted_at per §16.
SELECT * FROM constitution_versions
WHERE organization_id = sqlc.arg(org_id)::uuid
ORDER BY adopted_at DESC, version DESC LIMIT 1;

-- name: UpsertMandate :exec
-- committees only — the service validates org_units.type='committee'.
INSERT INTO committee_mandates (org_unit_id, mandate, reports_to, budget_limit_minor, currency, active)
VALUES (sqlc.arg(unit_id)::uuid, sqlc.arg(mandate), sqlc.narg(reports_to)::uuid,
        sqlc.narg(budget_limit_minor), sqlc.narg(currency), sqlc.arg(active))
ON CONFLICT (org_unit_id) DO UPDATE SET
  mandate = EXCLUDED.mandate, reports_to = EXCLUDED.reports_to,
  budget_limit_minor = EXCLUDED.budget_limit_minor, currency = EXCLUDED.currency,
  active = EXCLUDED.active;

-- name: GetMandate :one
SELECT * FROM committee_mandates WHERE org_unit_id = sqlc.arg(unit_id)::uuid;

-- name: ListCommitteeMandates :many
SELECT cm.* FROM committee_mandates cm
JOIN org_units u ON u.id = cm.org_unit_id
WHERE u.organization_id = sqlc.arg(org_id)::uuid AND cm.active
ORDER BY u.name;

-- name: GetCommitteeUnit :one
-- mandate upserts validate the target is a live committee in THIS org.
SELECT type FROM org_units
WHERE id = $1 AND organization_id = $2 AND archived_at IS NULL;

-- name: OrgTenantID :one
-- system-scoped task paths need the event tenant from the org.
SELECT tenant_id FROM organizations WHERE id = $1;

-- name: HandoverGeneratedExists :one
-- dedupe: one HandoverGenerated per term aggregate (Rule 27-style
-- idempotent producer — the sweep can re-run safely).
SELECT EXISTS(SELECT 1 FROM domain_events WHERE event_type = 'HandoverGenerated' AND aggregate_id = $1);

-- name: ExpiredCurrentTerms :many
-- governance:term_transitions sweep — current terms past ends_at.
SELECT * FROM terms WHERE is_current AND ends_at < CURRENT_DATE;

-- name: TermEndingSoon :many
-- governance:handover_reminders — terms ending inside the reminder window.
SELECT * FROM terms WHERE is_current
  AND ends_at BETWEEN CURRENT_DATE AND CURRENT_DATE + sqlc.arg(days)::int;

-- name: HandoverCompletedExists :one
SELECT EXISTS(SELECT 1 FROM domain_events
  WHERE event_type = 'HandoverCompleted' AND aggregate_id = $1) AS e;

-- name: GetMandateScoped :one
-- org-scoped variant for request paths — a foreign-org unit id must not
-- resolve a mandate (Rule 24).
SELECT cm.* FROM committee_mandates cm
JOIN org_units u ON u.id = cm.org_unit_id
WHERE cm.org_unit_id = sqlc.arg(unit_id)::uuid
  AND u.organization_id = sqlc.arg(org_id)::uuid;
