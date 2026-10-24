-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
-- Outbox + audit queries (Phase 02 emission, Phase 05 dispatch).

-- name: EmitDomainEvent :exec
INSERT INTO domain_events (name, aggregate_type, aggregate_id, tenant_id, actor_id, payload)
VALUES (sqlc.arg(name), sqlc.arg(aggregate_type), sqlc.arg(aggregate_id),
        sqlc.arg(tenant_id), sqlc.arg(actor_id), sqlc.arg(payload));

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (tenant_id, organization_id, actor_id, action, target_type, target_id, context)
VALUES (sqlc.arg(tenant_id), sqlc.arg(organization_id), sqlc.arg(actor_id),
        sqlc.arg(action), sqlc.arg(target_type), sqlc.arg(target_id), sqlc.arg(context));
