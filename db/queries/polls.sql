-- Ref: oga9-engineering/sections/26-polls-surveys.md — polls & surveys.
-- Secrecy is structural: poll_ballots = turnout receipt (one per member),
-- poll_votes = content with membership stamped only on non-secret polls.

-- ---------- polls ----------
-- name: CreatePoll :one
INSERT INTO polls (organization_id, created_by, question, kind, secret,
  audience, opens_at, closes_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(created_by), sqlc.arg(question),
  sqlc.arg(kind), sqlc.arg(secret), sqlc.arg(audience),
  sqlc.narg(opens_at)::timestamptz, sqlc.narg(closes_at)::timestamptz)
RETURNING *;

-- name: GetPoll :one
SELECT * FROM polls WHERE id = $1 AND organization_id = $2;

-- name: ListPolls :many
SELECT * FROM polls WHERE organization_id = $1
  AND ($2::text = '' OR state = $2)
ORDER BY created_at DESC LIMIT 500;

-- name: OpenPoll :one
-- draft → open; the resolved audience snapshot is written back into the
-- row (audience.resolved_members) — members joining after open are not
-- retro-enfranchised (§26).
UPDATE polls SET state = 'open', secret = $3, opens_at = now(),
  audience = audience || sqlc.arg(resolved_audience)::jsonb
WHERE id = $1 AND organization_id = $2 AND state = 'draft'
RETURNING *;

-- name: ClosePoll :one
UPDATE polls SET state = 'closed', closes_at = now()
WHERE id = $1 AND organization_id = $2 AND state = 'open'
RETURNING *;

-- name: DuePollsToClose :many
SELECT * FROM polls WHERE state = 'open' AND closes_at IS NOT NULL AND closes_at < now();

-- name: AddPollOption :one
INSERT INTO poll_options (poll_id, label, position)
VALUES (sqlc.arg(poll_id), sqlc.arg(label), sqlc.arg(position))
RETURNING *;

-- name: ListPollOptions :many
SELECT * FROM poll_options WHERE poll_id = $1 ORDER BY position;

-- name: PollOptionBelongs :one
SELECT EXISTS(SELECT 1 FROM poll_options o JOIN polls p ON p.id = o.poll_id
  WHERE o.id = $1 AND p.id = $2 AND p.organization_id = $3);

-- name: CastBallot :one
-- one vote per member — ON CONFLICT DO NOTHING makes replays surface as
-- already_voted via ErrNoRows.
INSERT INTO poll_ballots (poll_id, membership_id)
VALUES ($1, $2)
ON CONFLICT (poll_id, membership_id) DO NOTHING
RETURNING *;

-- name: HasBallot :one
SELECT EXISTS(SELECT 1 FROM poll_ballots WHERE poll_id = $1 AND membership_id = $2);

-- name: AddPollVote :exec
INSERT INTO poll_votes (poll_id, option_id, membership_id)
VALUES ($1, $2, $3);

-- name: PollTally :many
SELECT option_id, count(*)::int AS votes FROM poll_votes
WHERE poll_id = $1 GROUP BY option_id;

-- name: PollTurnout :one
SELECT count(*)::int FROM poll_ballots WHERE poll_id = $1;

-- name: PollVoters :many
-- non-secret polls only — per-option voter list for the results view.
SELECT v.option_id, v.membership_id, m.member_no FROM poll_votes v
JOIN organization_memberships m ON m.id = v.membership_id
WHERE v.poll_id = $1;

-- ---------- surveys ----------
-- name: CreateSurvey :one
INSERT INTO surveys (organization_id, created_by, title, intro, schema,
  anonymous, audience, opens_at, closes_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(created_by), sqlc.arg(title),
  sqlc.narg(intro)::text, sqlc.arg(schema), sqlc.arg(anonymous),
  sqlc.arg(audience),
  sqlc.narg(opens_at)::timestamptz, sqlc.narg(closes_at)::timestamptz)
RETURNING *;

-- name: GetSurvey :one
SELECT * FROM surveys WHERE id = $1 AND organization_id = $2;

-- name: ListSurveys :many
SELECT * FROM surveys WHERE organization_id = $1
  AND ($2::text = '' OR state = $2)
ORDER BY created_at DESC LIMIT 500;

-- name: OpenSurvey :one
UPDATE surveys SET state = 'open', anonymous = $3, opens_at = now(),
  audience = audience || sqlc.arg(resolved_audience)::jsonb
WHERE id = $1 AND organization_id = $2 AND state = 'draft'
RETURNING *;

-- name: CloseSurvey :one
UPDATE surveys SET state = 'closed', closes_at = now()
WHERE id = $1 AND organization_id = $2 AND state = 'open'
RETURNING *;

-- name: DueSurveysToClose :many
SELECT * FROM surveys WHERE state = 'open' AND closes_at IS NOT NULL AND closes_at < now();

-- name: SubmitSurveyResponse :one
-- one response per member on named surveys (NULL members don't dedupe at
-- the DB — anonymous dedupe rides the oga9:survey:claim marker).
INSERT INTO survey_responses (survey_id, membership_id, answers)
VALUES ($1, $2, $3)
ON CONFLICT (survey_id, membership_id) DO NOTHING
RETURNING *;

-- name: MySurveyResponse :one
SELECT * FROM survey_responses WHERE survey_id = $1 AND membership_id = $2;

-- name: SurveyResponseCount :one
SELECT count(*)::int FROM survey_responses WHERE survey_id = $1;

-- name: ListSurveyResponses :many
SELECT * FROM survey_responses WHERE survey_id = $1
ORDER BY submitted_at LIMIT 5000;
