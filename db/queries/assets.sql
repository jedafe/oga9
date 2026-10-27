-- Ref: oga9-engineering/sections/18-assets.md

-- name: CreateAsset :one
INSERT INTO assets (organization_id, category, name, description, identifier,
  condition, acquired_at, cost_minor, currency, unit_id)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(category), sqlc.arg(name),
  sqlc.narg(description), sqlc.narg(identifier), sqlc.arg(condition),
  sqlc.narg(acquired_at), sqlc.narg(cost_minor), sqlc.narg(currency),
  sqlc.narg(unit_id)::uuid)
RETURNING id;

-- name: GetAsset :one
SELECT * FROM assets WHERE id = $1 AND organization_id = $2;

-- name: ListAssets :many
SELECT * FROM assets WHERE organization_id = sqlc.arg(org_id)::uuid
  AND (sqlc.narg(state)::text = '' OR state = sqlc.narg(state)::text)
  AND (sqlc.narg(category)::text = '' OR category = sqlc.narg(category)::text)
ORDER BY created_at DESC LIMIT 500;

-- name: SetAssetState :execrows
UPDATE assets SET state = $2, condition = $3
WHERE id = $1 AND state = $4;

-- name: PatchAsset :execrows
UPDATE assets SET
  name = COALESCE(sqlc.narg(name), name),
  description = COALESCE(sqlc.narg(description), description),
  condition = COALESCE(sqlc.narg(condition), condition),
  unit_id = COALESCE(sqlc.narg(unit_id)::uuid, unit_id)
WHERE id = sqlc.arg(id)::uuid AND state <> 'disposed';

-- name: CurrentCustody :one
SELECT * FROM custody_assignments
WHERE asset_id = $1 AND returned_at IS NULL
ORDER BY assigned_at DESC LIMIT 1;

-- name: AssignCustody :one
INSERT INTO custody_assignments (asset_id, holder_member_id, holder_unit_id,
  term_id, assigned_by)
VALUES (sqlc.arg(asset_id)::uuid, sqlc.narg(holder_member_id)::uuid,
  sqlc.narg(holder_unit_id)::uuid, sqlc.narg(term_id)::uuid,
  sqlc.arg(assigned_by)::uuid)
RETURNING id;

-- name: ReturnCustody :execrows
UPDATE custody_assignments SET returned_at = now()
WHERE id = $1 AND returned_at IS NULL;

-- name: UnreturnedCustodyInTerm :many
-- handover sweep feed: custody still out when the term ended.
SELECT ca.*, a.name AS asset_name, a.organization_id
FROM custody_assignments ca JOIN assets a ON a.id = ca.asset_id
WHERE ca.term_id = $1 AND ca.returned_at IS NULL;

-- name: ListCustody :many
SELECT * FROM custody_assignments WHERE asset_id = $1 ORDER BY assigned_at DESC;

-- name: LogMaintenance :one
INSERT INTO maintenance_records (asset_id, kind, detail, cost_minor, currency, next_due, logged_by)
VALUES (sqlc.arg(asset_id)::uuid, sqlc.arg(kind), sqlc.narg(detail),
  sqlc.narg(cost_minor), sqlc.narg(currency), sqlc.narg(next_due),
  sqlc.arg(logged_by)::uuid)
RETURNING id;

-- name: MaintenanceDue :many
-- weekly sweep: next_due in the past/within the lookahead window.
SELECT mr.*, a.organization_id, a.name AS asset_name
FROM maintenance_records mr
JOIN assets a ON a.id = mr.asset_id
WHERE a.state <> 'disposed' AND mr.next_due <= now()::date + $1::int;

-- name: ListMaintenance :many
SELECT * FROM maintenance_records WHERE asset_id = $1 ORDER BY logged_at DESC;

-- name: CreateAudit :one
INSERT INTO asset_audits (organization_id, started_by)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(started_by)::uuid)
RETURNING id;

-- name: GetAudit :one
SELECT * FROM asset_audits WHERE id = $1 AND organization_id = $2;

-- name: ListAudits :many
SELECT * FROM asset_audits WHERE organization_id = $1 ORDER BY created_at DESC LIMIT 100;

-- name: CompleteAudit :execrows
UPDATE asset_audits SET findings = $2, state = 'completed'
WHERE id = $1 AND state = 'open';

-- name: UpsertCredential :exec
INSERT INTO digital_credentials (asset_id, kind, label, reference, vault_ref, updated_at)
VALUES (sqlc.arg(asset_id)::uuid, sqlc.arg(kind), sqlc.arg(label),
  sqlc.arg(reference), sqlc.narg(vault_ref), now())
ON CONFLICT (id) DO NOTHING;

-- name: ListCredentials :many
SELECT * FROM digital_credentials WHERE asset_id = $1;

-- name: DeleteCredential :execrows
DELETE FROM digital_credentials WHERE id = $1 AND asset_id = $2;
