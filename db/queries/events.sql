-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
-- Outbox + audit queries (Phase 02 emission, Phase 05 dispatch).

-- name: EmitDomainEvent :exec
INSERT INTO domain_events (event_type, aggregate_type, aggregate_id, tenant_id, organization_id, actor_id, payload, sourced)
VALUES (sqlc.arg(event_type), sqlc.arg(aggregate_type), sqlc.arg(aggregate_id),
        sqlc.arg(tenant_id), sqlc.arg(organization_id), sqlc.arg(actor_id),
        sqlc.arg(payload), sqlc.arg(sourced));

-- name: InsertAuditEvent :exec
INSERT INTO audit_events (tenant_id, organization_id, actor_id, action, target_type, target_id, context)
VALUES (sqlc.arg(tenant_id), sqlc.arg(organization_id), sqlc.arg(actor_id),
        sqlc.arg(action), sqlc.arg(target_type), sqlc.arg(target_id), sqlc.arg(context));

-- name: FetchPendingEvents :many
-- Outbox claim set: undelivered events under the attempt cap, oldest first.
SELECT id, event_type, aggregate_type, aggregate_id, tenant_id, organization_id,
       actor_id, payload, occurred_at, attempts
FROM domain_events
WHERE dispatched_at IS NULL AND attempts < sqlc.arg(max_attempts)::int
ORDER BY occurred_at
LIMIT sqlc.arg(batch_size)::int;

-- name: MarkEventDispatched :exec
UPDATE domain_events SET dispatched_at = now() WHERE id = $1;

-- name: RecordEventAttempt :exec
-- Failed consumer pass — stays pending, retried on the next dispatch tick.
UPDATE domain_events SET attempts = attempts + 1, last_error = $2 WHERE id = $1;

-- name: DeadLetterEvent :exec
-- Attempt cap reached: pull the event out of the pending set and record the
-- terminal error. `events:retry_dead` (Phase 06) can reset dispatched_at.
UPDATE domain_events SET dispatched_at = now(), attempts = attempts + 1, last_error = $2
WHERE id = $1;

-- name: ConsumerLogged :one
SELECT EXISTS(SELECT 1 FROM event_consumer_log WHERE consumer = $1 AND event_id = $2) AS processed;

-- name: LogConsumerProcessed :exec
-- Inserted in the SAME tx as the consumer's own writes (§05 contract).
INSERT INTO event_consumer_log (consumer, event_id) VALUES ($1, $2)
ON CONFLICT (consumer, event_id) DO NOTHING;

-- name: LinkEntities :exec
INSERT INTO entity_links (organization_id, source_type, source_id, target_type, target_id, relationship_type, created_by)
VALUES (sqlc.arg(organization_id), sqlc.arg(source_type), sqlc.arg(source_id),
        sqlc.arg(target_type), sqlc.arg(target_id), sqlc.arg(relationship_type), sqlc.arg(created_by))
ON CONFLICT (source_type, source_id, target_type, target_id, relationship_type) DO NOTHING;

-- name: RelatedEntities :many
-- ADR-015 "related" helper: bidirectional traversal of entity_links from a
-- (type,id) seed, depth-capped to keep the walk bounded.
WITH RECURSIVE rel AS (
  SELECT el.target_type AS node_type, el.target_id AS node_id, el.relationship_type, 1 AS depth
  FROM entity_links el
  WHERE el.source_type = $1 AND el.source_id = $2::uuid
  UNION
  SELECT CASE WHEN el.source_type = r.node_type AND el.source_id = r.node_id
              THEN el.target_type ELSE el.source_type END,
         CASE WHEN el.source_type = r.node_type AND el.source_id = r.node_id
              THEN el.target_id ELSE el.source_id END,
         el.relationship_type, r.depth + 1
  FROM entity_links el
  JOIN rel r ON (el.source_type = r.node_type AND el.source_id = r.node_id)
            OR (el.target_type = r.node_type AND el.target_id = r.node_id)
  WHERE r.depth < 5
)
SELECT DISTINCT node_type, node_id, relationship_type FROM rel;
