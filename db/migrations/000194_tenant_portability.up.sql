-- Ref: oga9-engineering/adr/ADR-026-tenant-portability-migration.md +
-- sections/15-subscription-admin.md — tenant_exports gains the migration
-- contract columns; org.admin.export seeds for the owner-org admin.
ALTER TABLE tenant_exports
  ADD COLUMN options jsonb NOT NULL DEFAULT '{}'::jsonb,
  ADD COLUMN bundle_version int,
  ADD COLUMN confirmed_at timestamptz;

INSERT INTO permissions (node, label) VALUES
('org.admin.export','Export tenant / migrate to self-hosted')
ON CONFLICT (node) DO NOTHING;

-- org_admin holds a wildcard org.% grant from 000019 but that INSERT ran
-- at seed time — existing installs need the row explicitly.
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'org.admin.export' FROM user_roles r WHERE r.slug = 'org_admin'
ON CONFLICT DO NOTHING;
