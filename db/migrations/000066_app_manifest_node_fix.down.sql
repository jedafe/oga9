UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.members.view","org.members.manage","org.admin.settings","org.admin.branding","org.directory.manage"]')
WHERE app_id = 'organization_core';
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.comms.view","org.comms.broadcast","org.documents.view_any","org.documents.manage"]')
WHERE app_id = 'comms_documents';
