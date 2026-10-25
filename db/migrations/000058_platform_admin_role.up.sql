-- Ref: oga9-engineering/conventions/permission-key-naming.md
-- platform.* nodes were seeded without a role granting them — add the
-- NULL-tenant platform_admin role so platform routes are reachable. Users
-- are attached via user_role_assignments (control-plane flow, §15).
INSERT INTO user_roles (tenant_id, name, slug, is_system)
VALUES (NULL, 'Platform Admin', 'platform_admin', true);
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'platform_admin' AND r.tenant_id IS NULL AND p.node LIKE 'platform.%';
