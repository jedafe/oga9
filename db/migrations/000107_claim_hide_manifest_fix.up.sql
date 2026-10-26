-- Ref: oga9-engineering/sections/12-finance-lite.md (drive transparency),
-- conventions/permission-key-naming.md (manifest node drift fixes)
-- 1) §12: "members can hide per-claim display" — per-claim contributor-hide
--    flag consumed by the drive contributions read-model.
ALTER TABLE payment_claims
  ADD COLUMN hide_contributor boolean NOT NULL DEFAULT false;

-- 2) app_registry manifest drift: seed claimed nodes that aren't catalogued.
--    org.documents.manage -> org.documents.view_any (closest catalogued node);
--    org.directory.manage -> org.listing.manage (§10/ADR-021 catalogued name).
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.comms.view","org.comms.broadcast","org.documents.view_any","org.documents.upload"]')
WHERE app_id = 'comms_documents';
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.members.view","org.members.manage","org.admin.settings","org.admin.branding","org.listing.manage"]')
WHERE app_id = 'organization_core';
