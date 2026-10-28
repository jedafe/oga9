DELETE FROM role_permissions WHERE permission = 'org.admin.export';
DELETE FROM permissions WHERE node = 'org.admin.export';
ALTER TABLE tenant_exports
  DROP COLUMN options,
  DROP COLUMN bundle_version,
  DROP COLUMN confirmed_at;
