DELETE FROM document_versions v USING documents d
WHERE v.document_id = d.id AND v.version = 1 AND d.version = 1;
