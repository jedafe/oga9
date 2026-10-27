-- Ref: oga9-engineering/sections/21-elections.md

-- name: CreateElection :one
INSERT INTO elections (organization_id, term_id, title, eligibility)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(term_id)::uuid, sqlc.arg(title), sqlc.arg(eligibility))
RETURNING id;

-- name: GetElection :one
SELECT * FROM elections WHERE id = $1 AND organization_id = $2;

-- name: GetElectionForUpdate :one
-- vote serialization: the election row lock orders concurrent ballot
-- writes so cast_seq stays gap-free.
SELECT id, state FROM elections WHERE id = $1 FOR UPDATE;

-- name: ListElections :many
SELECT * FROM elections WHERE organization_id = $1 ORDER BY created_at DESC LIMIT 100;

-- name: SetElectionState :execrows
UPDATE elections SET state = $2 WHERE id = $1 AND state = $3;

-- name: ScheduleElection :execrows
-- partial-patch semantics: unset timestamps keep their previous value.
UPDATE elections SET
  nominations_open = COALESCE($2, nominations_open),
  voting_opens = COALESCE($3, voting_opens),
  voting_closes = COALESCE($4, voting_closes)
WHERE id = $1 AND state IN ('setup','nominations','campaign');

-- name: ElectionsToOpenNominations :many
SELECT * FROM elections WHERE state = 'setup' AND nominations_open <= now();

-- name: ElectionsToOpenVoting :many
SELECT * FROM elections WHERE state IN ('nominations','campaign') AND voting_opens <= now();

-- name: ElectionsToClose :many
SELECT * FROM elections WHERE state = 'voting' AND voting_closes <= now();

-- name: ElectionsReminderWindow :many
-- elections:reminders — nominations/voting opening or closing inside the
-- next day.
SELECT * FROM elections WHERE
  (state = 'setup' AND nominations_open <= now() + interval '1 day')
  OR (state IN ('nominations','campaign') AND voting_opens <= now() + interval '1 day')
  OR (state = 'voting' AND voting_closes <= now() + interval '1 day');

-- name: NominateCandidate :one
INSERT INTO candidates (election_id, position_id, membership_id, scope_unit_id, manifesto, nominated_by)
VALUES (sqlc.arg(election_id)::uuid, sqlc.arg(position_id)::uuid,
  sqlc.arg(membership_id)::uuid, sqlc.narg(scope_unit_id)::uuid,
  sqlc.narg(manifesto), sqlc.arg(nominated_by)::uuid)
RETURNING id;

-- name: GetCandidate :one
SELECT * FROM candidates WHERE id = $1 AND election_id = $2;

-- name: ListCandidates :many
SELECT c.*, p.name AS position_name, m.member_no, u.type AS scope_type
FROM candidates c
JOIN org_positions p ON p.id = c.position_id
JOIN organization_memberships m ON m.id = c.membership_id
LEFT JOIN org_units u ON u.id = c.scope_unit_id
WHERE c.election_id = $1
ORDER BY c.position_id, c.state DESC;

-- name: OrgUnitType :one
-- scope_unit_id → appointment scope column ('branch'| 'committee'|'hq').
SELECT type FROM org_units WHERE id = $1 AND organization_id = $2;

-- name: SetCandidateState :execrows
UPDATE candidates SET state = $3 WHERE id = $1 AND election_id = $2 AND state = 'nominated';

-- name: OrgPositions :many
-- the ballot's seats — org-defined positions (Rule 20: positions carry
-- seats; nodes are separate).
SELECT id, name, seats FROM org_positions
WHERE organization_id = $1 ORDER BY name;

-- name: InsertBallot :one
-- UNIQUE(election_id,membership_id) — a duplicate is already_voted.
INSERT INTO ballots (election_id, membership_id)
VALUES (sqlc.arg(election_id)::uuid, sqlc.arg(membership_id)::uuid)
RETURNING id;

-- name: BallotExists :one
SELECT EXISTS(SELECT 1 FROM ballots WHERE election_id = $1 AND membership_id = $2);

-- name: InsertVote :exec
-- ANONYMOUS: no ballot_id / membership_id / user_id by design (secrecy
-- is structural — see the schema comment in the migration).
INSERT INTO votes (election_id, position_id, candidate_id, cast_seq)
VALUES (sqlc.arg(election_id)::uuid, sqlc.arg(position_id)::uuid,
  sqlc.narg(candidate_id)::uuid, sqlc.arg(cast_seq));

-- name: NextCastSeq :one
-- under the election row lock — gap-free ordering nonce.
SELECT COALESCE(max(cast_seq), 0) + 1 FROM votes WHERE election_id = $1;

-- name: TallyByPosition :many
SELECT position_id, candidate_id, count(*)::bigint AS votes
FROM votes WHERE election_id = $1
GROUP BY position_id, candidate_id;

-- name: ReminderSentRecently :one
-- elections:reminders dedupe — one reminder per election+state per day,
-- not one per sweep tick.
SELECT EXISTS(SELECT 1 FROM domain_events
  WHERE event_type = 'ElectionReminder' AND aggregate_id = $1
    AND occurred_at > now() - interval '1 day'
    AND payload->>'state' = $2);

-- name: ElectionAuditTrail :many
-- every nomination/vet/vote/ratify writes audit_events — the audit view.
SELECT * FROM audit_events
WHERE organization_id = $1 AND target_id = $2
ORDER BY created_at;

-- name: AppointmentExists :one
-- re-ratify idempotency: a winner already seated for this
-- term+position+scope is skipped, not re-appointed.
SELECT EXISTS(SELECT 1 FROM officer_appointments
  WHERE term_id = $1 AND position_id = $2 AND member_id = $3
    AND branch_id IS NOT DISTINCT FROM $4
    AND committee_id IS NOT DISTINCT FROM $5);

-- name: TermDates :one
-- ratification seats winners at the incoming term's start.
SELECT starts_at, ends_at FROM terms WHERE id = $1 AND organization_id = $2;
