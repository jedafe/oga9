-- Ref: oga9-engineering/sections/04-tenancy-organizations.md, 10-organization-core.md
-- sqlc queries for tenants, organizations, memberships, units, settings,
-- invitations, branding (Phase 03).

-- name: CreateTenant :one
INSERT INTO tenants (name, slug, billing_email)
VALUES (sqlc.arg(name), sqlc.arg(slug), sqlc.arg(billing_email))
RETURNING id;

-- name: SetTenantOwnerOrg :exec
UPDATE tenants SET owner_org_id = $2 WHERE id = $1;

-- name: CreateOrg :one
INSERT INTO organizations (tenant_id, name, slug, template_slug, org_type, country_code, city)
VALUES (sqlc.arg(tenant_id), sqlc.arg(name), sqlc.arg(slug), sqlc.arg(template_slug),
        sqlc.arg(org_type), sqlc.arg(country_code), sqlc.arg(city))
RETURNING id, name, slug, org_type;

-- name: GetOrg :one
SELECT id, tenant_id, name, slug, template_slug, org_type, country_code, city,
       is_public_listed, maturity_level
FROM organizations WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateOrgProfile :one
UPDATE organizations SET name = sqlc.arg(name), country_code = sqlc.arg(country_code),
       city = sqlc.arg(city), is_public_listed = sqlc.arg(is_public_listed)
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING id;

-- name: CountTenantOrgs :one
SELECT count(*) FROM organizations WHERE tenant_id = $1 AND deleted_at IS NULL;

-- name: ListOrgAdminTenants :many
-- Tenants where the caller holds the org_admin role — the server-side
-- "my tenant" resolution for POST /orgs (Rule 24).
SELECT DISTINCT r.tenant_id FROM user_role_assignments ura
JOIN user_roles r ON r.id = ura.role_id
WHERE ura.user_id = sqlc.arg(user_id) AND r.slug = 'org_admin' AND r.tenant_id IS NOT NULL;

-- name: HasRoleInTenant :one
SELECT EXISTS(
  SELECT 1 FROM user_role_assignments ura
  JOIN user_roles r ON r.id = ura.role_id
  WHERE ura.user_id = sqlc.arg(user_id) AND r.tenant_id = sqlc.arg(tenant_id) AND r.slug = sqlc.arg(slug)
);

-- name: GetTenant :one
SELECT id, name, slug, billing_email, edition, status FROM tenants WHERE id = $1;

-- name: ListUserOrgs :many
SELECT o.id, o.name, o.slug, o.org_type, o.tenant_id,
       m.id AS membership_id, m.category, m.lifecycle_state, m.member_no
FROM organization_memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.user_id = $1 AND o.deleted_at IS NULL
ORDER BY o.name;

-- name: GetMembership :one
SELECT m.id, m.organization_id, m.user_id, m.member_no, m.category, m.lifecycle_state
FROM organization_memberships m
JOIN organizations o ON o.id = m.organization_id
WHERE m.organization_id = $1 AND m.user_id = $2 AND o.deleted_at IS NULL;

-- name: GetMembershipByID :one
SELECT id, organization_id, user_id, member_no, category, lifecycle_state
FROM organization_memberships WHERE id = $1;

-- name: CreateMembership :one
-- joined_at marks actual joining — 'applied' states haven't joined yet (ADR-014).
INSERT INTO organization_memberships (organization_id, user_id, member_no, category, lifecycle_state, joined_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(user_id), sqlc.arg(member_no),
        sqlc.arg(category), sqlc.arg(lifecycle_state),
        CASE WHEN sqlc.arg(lifecycle_state) = 'active' THEN now() ELSE NULL END)
RETURNING id;

-- name: ListTemplates :many
SELECT slug, version, label FROM organization_templates ORDER BY label;

-- name: GetTemplate :one
SELECT seed FROM organization_templates WHERE slug = $1;

-- name: TemplateExists :one
SELECT EXISTS(SELECT 1 FROM organization_templates WHERE slug = $1) AS exists;

-- name: CloneTenantRoles :exec
-- Instantiate system role templates into tenant-scoped roles at org creation.
INSERT INTO user_roles (tenant_id, user_group_id, name, slug, is_system)
SELECT $1, r.user_group_id, r.name, r.slug, true FROM user_roles r WHERE r.tenant_id IS NULL;

-- name: CloneTenantRolePermissions :exec
INSERT INTO role_permissions (role_id, permission)
SELECT nr.id, rp.permission
FROM user_roles nr
JOIN user_roles sr ON sr.slug = nr.slug AND sr.tenant_id IS NULL
JOIN role_permissions rp ON rp.role_id = sr.id
WHERE nr.tenant_id = $1;

-- name: AssignRoleBySlug :exec
INSERT INTO user_role_assignments (user_id, role_id, granted_by)
SELECT $1, r.id, $3 FROM user_roles r WHERE r.tenant_id = $2 AND r.slug = $4
ON CONFLICT (user_id, role_id) DO NOTHING;

-- name: CreateUnit :one
INSERT INTO org_units (organization_id, parent_unit_id, type, level_label, name, slug)
VALUES (sqlc.arg(organization_id), sqlc.arg(parent_unit_id), sqlc.arg(type),
        sqlc.arg(level_label), sqlc.arg(name), sqlc.arg(slug))
RETURNING id;

-- name: ListUnits :many
SELECT id, parent_unit_id, type, level_label, name, slug, created_at
FROM org_units WHERE organization_id = $1 AND archived_at IS NULL
ORDER BY name;

-- name: GetUnit :one
SELECT id, organization_id, parent_unit_id, type, level_label, name, slug
FROM org_units WHERE id = $1 AND archived_at IS NULL;

-- name: UpdateUnit :one
UPDATE org_units SET name = $2, parent_unit_id = $3, type = $4, level_label = $5
WHERE id = $1 AND archived_at IS NULL
RETURNING id;

-- name: UnitHasActiveChildren :one
SELECT EXISTS (SELECT 1 FROM org_units WHERE parent_unit_id = $1 AND archived_at IS NULL) AS has_children;

-- name: UnitHasActiveMembers :one
-- §10 archive guard: a unit still holding committee members or open
-- officer appointments scoped to it counts as having dependents.
SELECT EXISTS (
  SELECT 1 FROM committee_memberships WHERE org_unit_id = $1
  UNION ALL
  SELECT 1 FROM officer_appointments
  WHERE (branch_id = $1 OR committee_id = $1)
    AND (ends_at IS NULL OR ends_at >= CURRENT_DATE)
) AS has_members;

-- name: ArchiveUnit :one
UPDATE org_units SET archived_at = now() WHERE id = $1 AND archived_at IS NULL
RETURNING id;

-- name: GetOrgSetting :one
SELECT value FROM org_settings WHERE organization_id = $1 AND key = $2;

-- name: ListOrgSettings :many
SELECT key, value, updated_at FROM org_settings WHERE organization_id = $1 ORDER BY key;

-- name: UpsertOrgSetting :exec
INSERT INTO org_settings (organization_id, key, value, updated_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(key), sqlc.arg(value), now())
ON CONFLICT (organization_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now();

-- name: CreateInvitation :one
INSERT INTO org_invitations (organization_id, email, phone, member_no_hint, category, invited_by, token, expires_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(email), sqlc.arg(phone), sqlc.arg(member_no_hint),
        sqlc.arg(category), sqlc.arg(invited_by), sqlc.arg(token),
        now() + interval '72 hours')
RETURNING id, token, expires_at;

-- name: ListPendingInvitations :many
SELECT id, email, phone, category, member_no_hint, expires_at, created_at
FROM org_invitations WHERE organization_id = $1 AND status = 'pending' AND expires_at > now()
ORDER BY created_at DESC;

-- name: GetInvitationByToken :one
SELECT id, organization_id, invited_by, email, category, member_no_hint, status, expires_at
FROM org_invitations WHERE token = $1;

-- name: SetInvitationStatus :one
-- Org-scoped: an invitation id alone must not grant cross-org mutation.
UPDATE org_invitations SET status = $3 WHERE id = $1 AND organization_id = $2 AND status = 'pending'
RETURNING id;

-- name: UpsertBranding :exec
INSERT INTO org_branding (organization_id, primary_color, secondary_color, motto, founded_year, portal_config)
VALUES (sqlc.arg(organization_id), sqlc.arg(primary_color), sqlc.arg(secondary_color),
        sqlc.arg(motto), sqlc.arg(founded_year), sqlc.arg(portal_config))
ON CONFLICT (organization_id) DO UPDATE SET
  primary_color = EXCLUDED.primary_color, secondary_color = EXCLUDED.secondary_color,
  motto = EXCLUDED.motto, founded_year = EXCLUDED.founded_year,
  portal_config = EXCLUDED.portal_config;

-- name: GetBranding :one
SELECT primary_color, secondary_color, motto, founded_year, portal_config
FROM org_branding WHERE organization_id = $1;

-- name: CreateCustomFieldGroup :one
INSERT INTO custom_field_groups (organization_id, entity_type, name, slug, sort_order, visibility, source)
VALUES (sqlc.arg(organization_id), sqlc.arg(entity_type), sqlc.arg(name), sqlc.arg(slug),
        sqlc.arg(sort_order), sqlc.arg(visibility), sqlc.arg(source))
RETURNING id;

-- name: CreateCustomField :exec
INSERT INTO custom_fields (group_id, organization_id, entity_type, key, label, help_text,
                           kind, required, options, sort_order, visibility, source)
VALUES (sqlc.arg(group_id), sqlc.arg(organization_id), sqlc.arg(entity_type), sqlc.arg(key),
        sqlc.arg(label), sqlc.arg(help_text), sqlc.arg(kind), sqlc.arg(required),
        sqlc.arg(options), sqlc.arg(sort_order), sqlc.arg(visibility), sqlc.arg(source));

-- name: ListPublicOrgs :many
SELECT id, name, slug, org_type, country_code, city
FROM organizations WHERE is_public_listed = true AND deleted_at IS NULL
ORDER BY name LIMIT 100;
