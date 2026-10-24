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

-- name: CloseTerm :exec
-- Closes the current term; appointments lose their is_current backing.
UPDATE terms SET is_current = false
WHERE id = sqlc.arg(id) AND organization_id = sqlc.arg(org_id)::uuid AND is_current;

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
RETURNING member_id;
