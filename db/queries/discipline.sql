-- Ref: oga9-engineering/sections/16-governance.md — discipline casework.

-- name: CreateDisciplineCase :one
INSERT INTO discipline_cases (organization_id, subject_id, reported_by, kind, title, details)
VALUES ($1, $2, sqlc.narg(reported_by), $3, $4, sqlc.narg(details))
RETURNING id;

-- name: GetDisciplineCase :one
SELECT * FROM discipline_cases WHERE id = $1 AND organization_id = $2;

-- name: ListDisciplineCases :many
SELECT * FROM discipline_cases WHERE organization_id = $1
  AND ($2::text = '' OR state = $2) ORDER BY created_at DESC;

-- name: MyDisciplineCases :many
SELECT c.* FROM discipline_cases c
JOIN organization_memberships m ON m.id = c.subject_id
WHERE m.user_id = $1 ORDER BY c.created_at DESC;

-- name: SetDisciplineCaseState :execrows
-- CAS transitions only — every move validates the expected from-state.
UPDATE discipline_cases SET state = $2 WHERE id = $1 AND state = $3;

-- name: ResolveDisciplineCase :execrows
UPDATE discipline_cases SET state = 'resolved', sanction = $2,
  fine_minor = sqlc.narg(fine_minor), currency = sqlc.narg(currency),
  resolved_at = now()
WHERE id = $1 AND state IN ('investigating','hearing');

-- name: CreateHearing :one
INSERT INTO discipline_hearings (case_id, scheduled_at)
VALUES ($1, sqlc.narg(scheduled_at))
RETURNING id;

-- name: ListHearings :many
SELECT * FROM discipline_hearings WHERE case_id = $1 ORDER BY created_at;

-- name: CompleteHearing :execrows
UPDATE discipline_hearings SET held_at = now(), outcome = $2,
  notes = sqlc.narg(notes)
WHERE id = $1 AND case_id = $3 AND held_at IS NULL;

-- name: FileAppeal :one
INSERT INTO discipline_appeals (case_id, filed_by, grounds)
VALUES ($1, $2, $3)
RETURNING id;

-- name: GetAppeal :one
SELECT a.* FROM discipline_appeals a
JOIN discipline_cases c ON c.id = a.case_id
WHERE a.id = $1 AND c.organization_id = $2;

-- name: ListAppeals :many
SELECT * FROM discipline_appeals WHERE case_id = $1 ORDER BY created_at;

-- name: DecideAppeal :execrows
UPDATE discipline_appeals SET state = $2, decided_at = now()
WHERE id = $1 AND state = 'filed';
