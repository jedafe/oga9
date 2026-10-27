-- Ref: oga9-engineering/sections/14-comms-documents.md — Upload never
-- wrote a document_versions row for v1, and AddDocumentVersion overwrote
-- documents.storage_key on bump: the original blob became unreachable
-- ("version 2 → prior version preserved" failed). Backfill v1 rows for
-- documents still AT version 1 (their storage_key is still the original).
-- Docs already bumped past v1 can't be recovered — the v1 key was lost.
INSERT INTO document_versions (document_id, version, storage_key, created_by)
SELECT d.id, 1, d.storage_key, d.uploaded_by FROM documents d
WHERE d.version = 1 AND NOT EXISTS (
  SELECT 1 FROM document_versions v WHERE v.document_id = d.id AND v.version = 1);
