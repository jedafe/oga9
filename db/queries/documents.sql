-- Ref: oga9-engineering/sections/14-comms-documents.md — documents.

-- name: CreateDocument :one
INSERT INTO documents (organization_id, kind, title, storage_key, visibility, unit_id, uploaded_by)
VALUES (sqlc.arg(organization_id), sqlc.arg(kind), sqlc.arg(title),
  sqlc.arg(storage_key), sqlc.arg(visibility), sqlc.narg(unit_id)::uuid, sqlc.arg(uploaded_by))
RETURNING *;

-- name: ListDocuments :many
SELECT * FROM documents WHERE organization_id = $1 AND deleted_at IS NULL
  AND ($2::text = '' OR kind = $2)
ORDER BY created_at DESC LIMIT 200;

-- name: GetDocument :one
SELECT * FROM documents WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL;

-- name: GetDocumentAny :one
SELECT * FROM documents WHERE id = $1;

-- name: PatchDocument :execrows
UPDATE documents SET
  title = COALESCE(sqlc.narg(title)::text, title),
  visibility = COALESCE(sqlc.narg(visibility)::text, visibility),
  unit_id = CASE WHEN sqlc.arg(clear_unit)::bool THEN NULL
    ELSE COALESCE(sqlc.narg(unit_id)::uuid, unit_id) END
WHERE id = sqlc.arg(id) AND organization_id = sqlc.arg(organization_id)
  AND deleted_at IS NULL;

-- name: SoftDeleteDocument :execrows
UPDATE documents SET deleted_at = now()
WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL;

-- name: AddDocumentVersion :one
-- bump documents.version + record the version row atomically in the
-- caller's tx.
UPDATE documents SET version = version + 1, storage_key = $3
WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL
RETURNING version;

-- name: InsertVersionRow :exec
INSERT INTO document_versions (document_id, version, storage_key, created_by)
VALUES ($1, $2, $3, $4);

-- name: DocumentVersions :many
SELECT * FROM document_versions WHERE document_id = $1 ORDER BY version DESC;

-- name: PublicDocuments :many
-- directory surface honors the same opt-in as /apply: an org that never
-- listed itself exposes no public docs listing (§03 opt-in). Slim
-- projection — storage_key is internal, never shipped to anonymous clients.
SELECT d.id, d.kind, d.title, d.version, d.created_at
FROM documents d JOIN organizations o ON o.id = d.organization_id
WHERE o.slug = $1 AND o.is_public_listed AND o.deleted_at IS NULL
  AND d.visibility = 'public' AND d.deleted_at IS NULL
ORDER BY d.created_at DESC;

-- name: DropLatestVersion :one
-- documents:process_upload recovery — a poisoned new version drops its
-- version row and reverts the head to the previous version instead of
-- deleting the whole document (good v1 survives a bad v2).
DELETE FROM document_versions WHERE document_id = $1 AND version = $2
RETURNING storage_key;

-- name: RevertDocumentHead :exec
-- point the head back at a surviving version row (called after
-- DropLatestVersion; the caller passes the previous version's key).
UPDATE documents SET version = version - 1, storage_key = $3
WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL;

-- name: LatestVersionRow :one
SELECT version, storage_key FROM document_versions
WHERE document_id = $1 ORDER BY version DESC LIMIT 1;
