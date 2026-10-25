DELETE FROM role_permissions WHERE role_id IN
  (SELECT id FROM user_roles WHERE slug = 'platform_admin' AND tenant_id IS NULL);
DELETE FROM user_roles WHERE slug = 'platform_admin' AND tenant_id IS NULL;
