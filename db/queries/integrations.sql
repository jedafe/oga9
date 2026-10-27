-- Ref: oga9-engineering/sections/23-integrations.md

-- name: CreateConnector :one
INSERT INTO connectors (organization_id, kind, config, credential_ref)
VALUES (sqlc.narg(org_id)::uuid, sqlc.arg(kind), sqlc.arg(config), sqlc.narg(credential_ref))
RETURNING id;

-- name: ListConnectors :many
SELECT * FROM connectors WHERE organization_id = $1 ORDER BY created_at DESC;

-- name: GetConnector :one
SELECT * FROM connectors WHERE id = $1 AND (organization_id = $2 OR organization_id IS NULL);

-- name: ConnectorsByKind :many
-- webhook intake can't attribute the org until the signature verifies —
-- try every enabled connector's secret; the one that verifies IS the
-- org attribution.
SELECT * FROM connectors WHERE kind = $1 AND enabled;

-- name: ConnectorByID :one
-- sync-job delivery knows the connector from the job row — no org
-- predicate (the job's provenance is the scope proof).
SELECT * FROM connectors WHERE id = $1;

-- name: SetConnectorEnabled :execrows
UPDATE connectors SET enabled = $3 WHERE id = $1 AND organization_id = $2;

-- name: ConnectorByKind :one
-- platform-scope first, then org-specific override. A NULL org arg
-- matches ANY enabled connector of that kind (webhook intake doesn't
-- know the org until it parses the payload).
SELECT * FROM connectors
WHERE kind = $1 AND enabled
  AND (sqlc.narg(org_id)::uuid IS NULL OR organization_id IS NULL OR organization_id = sqlc.narg(org_id)::uuid)
ORDER BY (organization_id = sqlc.narg(org_id)::uuid) DESC NULLS LAST LIMIT 1;

-- name: InsertWebhookReceipt :one
INSERT INTO webhook_receipts (provider, raw, signature_ok)
VALUES (sqlc.arg(provider), sqlc.arg(raw), sqlc.arg(signature_ok))
RETURNING id;

-- name: MarkReceiptProcessed :execrows
UPDATE webhook_receipts SET processed_at = now() WHERE id = $1 AND processed_at IS NULL;

-- name: UnprocessedReceipts :many
SELECT * FROM webhook_receipts WHERE processed_at IS NULL
ORDER BY created_at LIMIT 100;

-- name: CreateSyncJob :one
INSERT INTO sync_jobs (connector_id, direction, cursor, detail)
VALUES (sqlc.arg(connector_id)::uuid, sqlc.arg(direction), sqlc.narg(cursor), sqlc.narg(detail))
RETURNING id;

-- name: FinishSyncJob :execrows
UPDATE sync_jobs SET state = $2, detail = $3, ran_at = now()
WHERE id = $1 AND state = 'queued';

-- name: QueuedSyncJobs :many
SELECT * FROM sync_jobs WHERE state = 'queued' ORDER BY created_at LIMIT 100;

-- name: ExternalIDExists :one
-- idempotent sync (Rule 27): an already-mapped external record is a
-- dedupe hit, not a re-push.
SELECT EXISTS(SELECT 1 FROM external_event_map
  WHERE external_type = $1 AND external_id = $2);

-- name: MapExternal :exec
INSERT INTO external_event_map (internal_type, internal_id, external_type, external_id)
VALUES (sqlc.arg(internal_type), sqlc.arg(internal_id)::uuid,
  sqlc.arg(external_type), sqlc.arg(external_id))
ON CONFLICT DO NOTHING;

-- name: InternalMapped :one
-- reverse lookup: has this org already synced to the external type?
SELECT EXISTS(SELECT 1 FROM external_event_map
  WHERE internal_type = $1 AND internal_id = $2 AND external_type = $3);

-- name: InsertAPIKey :one
-- tenant_id nullable — user self-managed keys are user-scoped.
INSERT INTO api_keys (tenant_id, user_id, prefix, hashed_key, scopes, rate_tier)
VALUES (sqlc.narg(tenant_id)::uuid, sqlc.narg(user_id)::uuid, sqlc.arg(prefix),
  sqlc.arg(hashed_key), sqlc.arg(scopes)::text[], sqlc.arg(rate_tier))
RETURNING id;

-- name: ListAPIKeysForUser :many
-- never return hashed_key — prefix is the display-safe field.
SELECT id, tenant_id, user_id, prefix, scopes, rate_tier, revoked_at, created_at
FROM api_keys WHERE user_id = $1 ORDER BY created_at DESC;

-- name: RevokeAPIKeyForUser :execrows
UPDATE api_keys SET revoked_at = now() WHERE id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- name: APIKeyByHash :one
SELECT * FROM api_keys WHERE hashed_key = $1;

-- name: PublicOrgBySlug :many
-- locator/detail read — opted-in fields only (Rule: public directory
-- exposes only opted-in fields).
SELECT o.id, o.name, o.slug, o.city, o.country_code, o.maturity_level,
  (SELECT count(*) FROM organization_memberships m WHERE m.organization_id = o.id
    AND m.lifecycle_state = 'active')::int AS member_count
FROM organizations o
WHERE o.slug = $1 AND o.is_public_listed AND o.deleted_at IS NULL;

-- name: PublicOrgNeedsSync :many
-- japa_sync_reconcile: listed orgs with no locator mapping yet.
SELECT o.id FROM organizations o
WHERE o.is_public_listed AND o.deleted_at IS NULL
  AND NOT EXISTS (SELECT 1 FROM external_event_map em
    WHERE em.internal_type = 'organization' AND em.internal_id = o.id
      AND em.external_type = 'japaconnect:locator')
LIMIT 200;
