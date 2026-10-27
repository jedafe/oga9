-- Ref: oga9-engineering/sections/19-analytics.md

-- name: UpsertSnapshot :exec
INSERT INTO dashboard_snapshots (organization_id, role, snapshot, refreshed_at)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(role), sqlc.arg(snapshot), now())
ON CONFLICT (organization_id, role) DO UPDATE SET
  snapshot = EXCLUDED.snapshot, refreshed_at = EXCLUDED.refreshed_at;

-- name: GetSnapshot :one
SELECT * FROM dashboard_snapshots
WHERE organization_id = $1 AND role = $2;

-- name: UpsertHealthScore :exec
INSERT INTO health_scores (organization_id, period, score, components, formula_version)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(period), sqlc.arg(score),
  sqlc.arg(components), sqlc.arg(formula_version))
ON CONFLICT (organization_id, period) DO UPDATE SET
  score = EXCLUDED.score, components = EXCLUDED.components,
  formula_version = EXCLUDED.formula_version, computed_at = now();

-- name: LatestHealthScore :one
SELECT * FROM health_scores WHERE organization_id = $1
ORDER BY period DESC LIMIT 1;

-- name: ListHealthScores :many
SELECT * FROM health_scores WHERE organization_id = $1
ORDER BY period DESC LIMIT 24;

-- name: InsertReportRun :one
INSERT INTO report_runs (organization_id, kind, period, params, document_id, generated_by)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(kind), sqlc.narg(period),
  sqlc.narg(params), sqlc.narg(document_id)::uuid, sqlc.narg(generated_by)::uuid)
RETURNING id;

-- name: ReportRunExists :one
SELECT EXISTS(SELECT 1 FROM report_runs
  WHERE organization_id = $1 AND kind = $2 AND period = $3);

-- name: ListReportRuns :many
SELECT * FROM report_runs WHERE organization_id = $1
ORDER BY created_at DESC LIMIT 50;

-- name: GetReportRun :one
SELECT * FROM report_runs WHERE id = $1 AND organization_id = $2;

-- name: EventCounts30d :many
-- the projection source: per-type domain_event counts over 30 days.
-- Dashboards read THIS (or the snapshot built from it) — never the raw
-- domain tables (§19 rule).
SELECT event_type::text, count(*)::bigint AS n FROM domain_events
WHERE organization_id = $1 AND occurred_at > now() - interval '30 days'
GROUP BY event_type ORDER BY n DESC;

-- name: ListActiveOrgIDs :many
-- sweep feed: every live org gets its snapshots/health refreshed.
SELECT id FROM organizations WHERE deleted_at IS NULL;

-- name: ListKpiDefs :many
-- org-defined KPIs win over global (NULL-org) defaults by key.
SELECT * FROM kpi_definitions
WHERE organization_id = $1 OR organization_id IS NULL
ORDER BY key, organization_id NULLS LAST;
