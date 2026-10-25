-- Ref: oga9-engineering/sections/03-rbac-positions.md (Phase 04)
-- RBAC + positions: permission resolution (group+direct+position+scope union),
-- positions, terms, officer appointments, committee memberships.

-- name: ResolveOrgPermissions :many
-- Union: tenant-scoped role grants + position grants via active
-- appointments in the org's current term. _branch/_committee nodes are
-- suffixed base nodes — resolved in Go against scoped grants.
SELECT DISTINCT permission FROM (
  SELECT rp.permission
  FROM user_role_assignments ura
  JOIN user_roles r ON r.id = ura.role_id
  JOIN role_permissions rp ON rp.role_id = r.id
  JOIN organizations o ON o.id = sqlc.arg(org_id)::uuid
  WHERE ura.user_id = sqlc.arg(user_id)::uuid AND r.tenant_id = o.tenant_id
  UNION
  -- §03 union term: the member's org group → roles bound to that group
  -- (organization_memberships.user_group_id is the group link). Roles are
  -- tenant-scoped: system templates (tenant_id NULL) or this org's tenant —
  -- cloned roles share user_group_id, so the scope check prevents
  -- cross-tenant grant leakage.
  SELECT rp.permission
  FROM organization_memberships m
  JOIN organizations o ON o.id = m.organization_id
  JOIN user_roles r ON r.user_group_id = m.user_group_id
                   AND (r.tenant_id IS NULL OR r.tenant_id = o.tenant_id)
  JOIN role_permissions rp ON rp.role_id = r.id
  WHERE m.organization_id = sqlc.arg(org_id)::uuid
    AND m.user_id = sqlc.arg(user_id)::uuid
  UNION
  SELECT pp.permission
  FROM officer_appointments oa
  JOIN terms t ON t.id = oa.term_id AND t.is_current
  JOIN organization_memberships m ON m.id = oa.member_id
  JOIN position_permissions pp ON pp.position_id = oa.position_id
  WHERE oa.organization_id = sqlc.arg(org_id)::uuid
    AND m.user_id = sqlc.arg(user_id)::uuid
    AND oa.starts_at <= CURRENT_DATE
    AND (oa.ends_at IS NULL OR oa.ends_at >= CURRENT_DATE)
);

-- name: ScopedPositionNodes :many
-- Position grants carrying a branch or committee scope — feeds _branch /
-- _committee suffix resolution.
SELECT pp.permission, oa.branch_id, oa.committee_id
FROM officer_appointments oa
JOIN terms t ON t.id = oa.term_id AND t.is_current
JOIN organization_memberships m ON m.id = oa.member_id
JOIN position_permissions pp ON pp.position_id = oa.position_id
WHERE oa.organization_id = sqlc.arg(org_id)::uuid
  AND m.user_id = sqlc.arg(user_id)::uuid
  AND oa.starts_at <= CURRENT_DATE
  AND (oa.ends_at IS NULL OR oa.ends_at >= CURRENT_DATE)
  AND (oa.branch_id IS NOT NULL OR oa.committee_id IS NOT NULL);

-- name: UserCommitteeUnits :many
SELECT cm.org_unit_id FROM committee_memberships cm
JOIN organization_memberships m ON m.id = cm.member_id
WHERE cm.organization_id = sqlc.arg(org_id)::uuid AND m.user_id = sqlc.arg(user_id)::uuid;

-- name: CreatePosition :one
INSERT INTO org_positions (organization_id, name, slug, is_executive, seats)
VALUES (sqlc.arg(organization_id), sqlc.arg(name), sqlc.arg(slug),
        sqlc.arg(is_executive), sqlc.arg(seats))
RETURNING id;

-- name: AssignPositionPermission :exec
INSERT INTO position_permissions (position_id, permission)
VALUES (sqlc.arg(position_id), sqlc.arg(permission))
ON CONFLICT (position_id, permission) DO NOTHING;

-- name: CreateTerm :one
INSERT INTO terms (organization_id, label, starts_at, ends_at, is_current)
VALUES (sqlc.arg(organization_id), sqlc.arg(label), sqlc.arg(starts_at),
        sqlc.arg(ends_at), sqlc.arg(is_current))
RETURNING id;

-- name: CloseTerm :one
-- Closes the current term; appointments lose their is_current backing.
-- RETURNING lets callers distinguish unknown/foreign/closed terms (404).
UPDATE terms SET is_current = false
WHERE id = sqlc.arg(id) AND organization_id = sqlc.arg(org_id)::uuid AND is_current
RETURNING id;

-- name: EndTermAppointments :many
-- Ends every open appointment of the term; RETURNING rows feed
-- AppointmentEnded events + permission-cache invalidation.
UPDATE officer_appointments SET ends_at = CURRENT_DATE
WHERE term_id = sqlc.arg(term_id)::uuid AND ends_at IS NULL
RETURNING id, position_id, member_id;

-- name: AppointOfficer :one
INSERT INTO officer_appointments (organization_id, term_id, position_id, member_id,
                                  branch_id, committee_id, starts_at)
VALUES (sqlc.arg(organization_id), sqlc.arg(term_id), sqlc.arg(position_id),
        sqlc.arg(member_id), sqlc.arg(branch_id), sqlc.arg(committee_id),
        sqlc.arg(starts_at))
RETURNING id;

-- name: EndAppointment :one
UPDATE officer_appointments SET ends_at = CURRENT_DATE
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(org_id)::uuid AND ends_at IS NULL
RETURNING id, position_id, member_id;

-- name: AddCommitteeMember :one
INSERT INTO committee_memberships (organization_id, org_unit_id, member_id, role)
VALUES (sqlc.arg(organization_id), sqlc.arg(org_unit_id), sqlc.arg(member_id), sqlc.arg(role))
RETURNING id;

-- name: RemoveCommitteeMember :one
DELETE FROM committee_memberships
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(org_id)::uuid
RETURNING member_id, org_unit_id;

-- name: GetPositionOrg :one
SELECT organization_id FROM org_positions WHERE id = sqlc.arg(id)::uuid;

-- name: ListPositions :many
SELECT id, name, slug, is_executive, seats, created_at FROM org_positions
WHERE organization_id = sqlc.arg(org_id)::uuid ORDER BY name;

-- name: ListTerms :many
SELECT id, label, starts_at, ends_at, is_current FROM terms
WHERE organization_id = sqlc.arg(org_id)::uuid ORDER BY starts_at DESC;

-- name: ListAppointments :many
-- Governance roster: appointment + position/term/officer display names.
SELECT a.id, a.term_id, a.position_id, a.member_id, a.branch_id, a.committee_id,
       a.starts_at, a.ends_at, p.name AS position_name, t.label AS term_label,
       u.display_name AS officer_name
FROM officer_appointments a
JOIN org_positions p ON p.id = a.position_id
JOIN terms t ON t.id = a.term_id
JOIN organization_memberships m ON m.id = a.member_id
JOIN users u ON u.id = m.user_id
WHERE a.organization_id = sqlc.arg(org_id)::uuid
ORDER BY a.starts_at DESC;

-- name: ListCommitteeMembers :many
SELECT cm.id, cm.member_id, cm.role, cm.created_at,
       u.id AS user_id, u.display_name AS member_name
FROM committee_memberships cm
JOIN organization_memberships m ON m.id = cm.member_id
JOIN users u ON u.id = m.user_id
WHERE cm.org_unit_id = sqlc.arg(unit_id)::uuid
  AND cm.organization_id = sqlc.arg(org_id)::uuid
ORDER BY cm.created_at;

-- name: GetTermOrg :one
SELECT organization_id, is_current FROM terms WHERE id = sqlc.arg(id)::uuid;

-- name: GetPositionSeats :one
SELECT seats FROM org_positions WHERE id = sqlc.arg(id)::uuid;

-- name: CountPositionSeats :one
-- Active seat occupancy for a position within a term (seats cap, §03).
SELECT count(*) FROM officer_appointments
WHERE position_id = sqlc.arg(position_id)::uuid AND term_id = sqlc.arg(term_id)::uuid
  AND (ends_at IS NULL OR ends_at >= CURRENT_DATE);

-- name: CloseCurrentTerm :one
-- Auto-close the org's live term when a new current term is created (§03).
UPDATE terms SET is_current = false
WHERE organization_id = sqlc.arg(org_id)::uuid AND is_current
RETURNING id;
