-- Ref: oga9-engineering/adr/ADR-016-rules-workflow-engine.md — rule_definitions.
-- Resolution: org-scoped active rule wins over the platform default row;
-- highest priority breaks ties (engine falls through to org_settings on miss).

-- name: GetRule :one
SELECT * FROM rule_definitions
WHERE key = $1 AND active
  AND (organization_id = $2 OR organization_id IS NULL)
ORDER BY (organization_id IS NULL) ASC, priority DESC
LIMIT 1;

-- name: ListRules :many
SELECT * FROM rule_definitions WHERE organization_id = $1 ORDER BY key;

-- name: UpsertOrgRule :one
INSERT INTO rule_definitions (organization_id, key, kind, definition, priority, active)
VALUES (sqlc.arg(organization_id), sqlc.arg(key), sqlc.arg(kind),
  sqlc.arg(definition), sqlc.arg(priority), sqlc.arg(active))
ON CONFLICT (organization_id, key) WHERE organization_id IS NOT NULL
DO UPDATE SET kind = EXCLUDED.kind, definition = EXCLUDED.definition,
  priority = EXCLUDED.priority, active = EXCLUDED.active
RETURNING *;

-- name: UpsertPlatformRule :one
INSERT INTO rule_definitions (organization_id, key, kind, definition, priority, active)
VALUES (NULL, sqlc.arg(key), sqlc.arg(kind),
  sqlc.arg(definition), sqlc.arg(priority), sqlc.arg(active))
ON CONFLICT (key) WHERE organization_id IS NULL
DO UPDATE SET kind = EXCLUDED.kind, definition = EXCLUDED.definition,
  priority = EXCLUDED.priority, active = EXCLUDED.active
RETURNING *;

-- name: RetireRule :execrows
UPDATE rule_definitions SET active = false WHERE id = $1 AND organization_id = $2;
