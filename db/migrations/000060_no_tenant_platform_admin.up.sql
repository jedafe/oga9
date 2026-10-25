-- Ref: oga9-engineering/conventions/permission-key-naming.md
-- platform_admin is control-plane only (NULL-tenant). CloneTenantRoles now
-- excludes platform.*-carrying roles; remove clones that already landed in
-- tenants — cascades clear their role_permissions + user_role_assignments.
DELETE FROM user_roles WHERE slug = 'platform_admin' AND tenant_id IS NOT NULL;
