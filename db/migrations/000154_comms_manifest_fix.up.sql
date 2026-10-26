-- Ref: oga9-engineering/sections/14+24 — comms_documents manifest drift:
-- the manifest predates the §14 document nodes and the §24 chat nodes
-- (same drift class as 000107/000108/000148).
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.comms.view","org.comms.announce","org.comms.broadcast",
    "org.documents.view","org.documents.view_own","org.documents.view_branch","org.documents.view_any",
    "org.documents.upload","org.documents.publish","org.documents.scope","org.documents.archive",
    "org.chat.use","org.chat.post","org.chat.channels.manage","org.chat.moderate",
    "org.voice.start","org.voice.join"]')
WHERE app_id = 'comms_documents';
