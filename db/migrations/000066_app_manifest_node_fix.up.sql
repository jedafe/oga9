-- Ref: oga9-engineering/conventions/permission-key-naming.md
-- Manifest seeds referenced uncatalogued nodes: org.directory.manage (the
-- catalogued directory node is org.listing.manage, ADR-021) and
-- org.documents.manage (the documents taxonomy is
-- view/upload/publish/scope/archive/view_any).
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.members.view","org.members.manage","org.admin.settings","org.admin.branding","org.listing.manage"]')
WHERE app_id = 'organization_core';
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.comms.view","org.comms.broadcast","org.documents.view_any","org.documents.upload"]')
WHERE app_id = 'comms_documents';
