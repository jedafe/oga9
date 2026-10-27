-- Ref: oga9-engineering/sections/20-projects.md

-- name: CreateProject :one
INSERT INTO projects (organization_id, title, description, unit_id, starts_on, target_on)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(title), sqlc.narg(description),
  sqlc.narg(unit_id)::uuid, sqlc.narg(starts_on), sqlc.narg(target_on))
RETURNING id;

-- name: GetProject :one
SELECT * FROM projects WHERE id = $1 AND organization_id = $2;

-- name: ListProjects :many
SELECT * FROM projects WHERE organization_id = sqlc.arg(org_id)::uuid
  AND (sqlc.narg(state)::text = '' OR state = sqlc.narg(state)::text)
ORDER BY created_at DESC LIMIT 200;

-- name: SetProjectState :execrows
UPDATE projects SET state = $2 WHERE id = $1 AND state = $3;

-- name: ApproveProject :execrows
-- resolution edge lands at approval time.
UPDATE projects SET state = 'approved', resolution_id = $2
WHERE id = $1 AND state = 'proposed';

-- name: CompleteProject :execrows
UPDATE projects SET state = 'completed', completed_on = CURRENT_DATE
WHERE id = $1 AND state IN ('active','suspended');

-- name: SetProjectProgress :exec
-- milestone-weighted progress, recomputed on each completion.
UPDATE projects SET progress_pct = $2 WHERE id = $1;

-- name: AddMilestone :one
INSERT INTO project_milestones (project_id, title, weight, due_on)
VALUES (sqlc.arg(project_id)::uuid, sqlc.arg(title), sqlc.arg(weight), sqlc.narg(due_on))
RETURNING id;

-- name: ListMilestones :many
SELECT * FROM project_milestones WHERE project_id = $1 ORDER BY title;

-- name: CompleteMilestone :execrows
UPDATE project_milestones SET state = 'completed', completed_on = CURRENT_DATE
WHERE id = $1 AND project_id = $2 AND state = 'pending';

-- name: MilestoneProgress :one
-- weighted completion ratio → project.progress_pct.
SELECT COALESCE(sum(weight) FILTER (WHERE state='completed'),0)::bigint AS done,
       COALESCE(sum(weight),0)::bigint AS total
FROM project_milestones WHERE project_id = $1;

-- name: AddUpdate :one
INSERT INTO project_updates (project_id, author_id, body, progress_pct)
VALUES (sqlc.arg(project_id)::uuid, sqlc.arg(author_id)::uuid, sqlc.arg(body), sqlc.narg(progress_pct))
RETURNING id;

-- name: ListUpdates :many
SELECT * FROM project_updates WHERE project_id = $1 ORDER BY created_at DESC LIMIT 100;

-- name: LatestUpdate :one
SELECT created_at FROM project_updates WHERE project_id = $1
ORDER BY created_at DESC LIMIT 1;

-- name: CreateVendor :one
INSERT INTO vendors (organization_id, name, contact, kind)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(name), sqlc.narg(contact), sqlc.narg(kind))
RETURNING id;

-- name: ListVendors :many
SELECT * FROM vendors WHERE organization_id = $1 ORDER BY name;

-- name: GetVendor :one
SELECT * FROM vendors WHERE id = $1 AND organization_id = $2;

-- name: AddDonor :one
INSERT INTO project_donors (project_id, donor_name, membership_id, amount_minor, currency, payment_id)
VALUES (sqlc.arg(project_id)::uuid, sqlc.arg(donor_name),
  sqlc.narg(membership_id)::uuid, sqlc.narg(amount_minor), sqlc.narg(currency),
  sqlc.narg(payment_id)::uuid)
RETURNING id;

-- name: ListDonors :many
SELECT * FROM project_donors WHERE project_id = $1 ORDER BY created_at DESC;

-- name: LinkEntitiesForProject :exec
-- direct edge write for graph completeness at close (report document).
INSERT INTO entity_links (organization_id, source_type, source_id, target_type, target_id, relationship_type, created_by)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(source_type), sqlc.arg(source_id)::uuid,
  sqlc.arg(target_type), sqlc.arg(target_id)::uuid, sqlc.arg(rel), sqlc.narg(created_by)::uuid)
ON CONFLICT DO NOTHING;

-- name: ProjectGraph :many
-- edges in both directions around the project node.
SELECT source_type, source_id, target_type, target_id, relationship_type
FROM entity_links
WHERE (source_type = 'project' AND source_id = $1)
   OR (target_type = 'project' AND target_id = $1)
ORDER BY relationship_type;

-- name: PublicOrgProjects :many
-- public showcase: non-abandoned projects, minimal fields.
SELECT p.id, p.title, p.description, p.state, p.target_on, p.completed_on
FROM projects p JOIN organizations o ON o.id = p.organization_id
WHERE o.slug = $1 AND p.state IN ('active','completed')
ORDER BY p.state DESC, p.created_at DESC LIMIT 50;

-- name: ResolutionOutcome :one
-- approval gate: the resolution must exist in this org and be passed.
SELECT outcome FROM resolutions WHERE id = $1 AND organization_id = $2;
