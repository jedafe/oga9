-- Ref: oga9-engineering/sections/22-ai.md

-- name: CreateConversation :one
INSERT INTO ai_conversations (user_id, organization_id, persona)
VALUES (sqlc.arg(user_id)::uuid, sqlc.narg(org_id)::uuid, sqlc.arg(persona))
RETURNING id;

-- name: GetConversation :one
SELECT * FROM ai_conversations WHERE id = $1 AND user_id = $2;

-- name: ListConversations :many
SELECT * FROM ai_conversations WHERE user_id = $1 AND organization_id = $2
ORDER BY created_at DESC LIMIT 50;

-- name: AddMessage :one
INSERT INTO ai_messages (conversation_id, role, body, citations, tokens_in, tokens_out)
VALUES (sqlc.arg(conversation_id)::uuid, sqlc.arg(role), sqlc.arg(body),
  sqlc.narg(citations), sqlc.narg(tokens_in), sqlc.narg(tokens_out))
RETURNING id;

-- name: ListMessages :many
SELECT * FROM ai_messages WHERE conversation_id = $1 ORDER BY created_at LIMIT 100;

-- name: CreateGeneration :one
INSERT INTO ai_generations (organization_id, author_id, kind, draft)
VALUES (sqlc.arg(org_id)::uuid, sqlc.arg(author_id)::uuid, sqlc.arg(kind), sqlc.arg(draft))
RETURNING id;

-- name: GetGeneration :one
SELECT * FROM ai_generations WHERE id = $1 AND organization_id = $2;

-- name: ListGenerations :many
SELECT * FROM ai_generations WHERE organization_id = sqlc.arg(org_id)::uuid
  AND (sqlc.narg(state)::text = '' OR state = sqlc.narg(state)::text)
ORDER BY created_at DESC LIMIT 100;

-- name: SetGenerationState :execrows
UPDATE ai_generations SET state = $3, reviewed_by = $4
WHERE id = $1 AND organization_id = $2 AND state = 'pending_review';

-- name: MarkGenerationApplied :execrows
-- approved → applied once the domain seam lands the draft.
UPDATE ai_generations SET state = 'applied'
WHERE id = $1 AND organization_id = $2 AND state = 'approved';

-- name: LogUsage :exec
INSERT INTO ai_usage_log (tenant_id, user_id, kind, tokens_in, tokens_out, cost_micro_usd)
VALUES (sqlc.arg(tenant_id)::uuid, sqlc.narg(user_id)::uuid, sqlc.arg(kind),
  sqlc.narg(tokens_in), sqlc.narg(tokens_out), sqlc.narg(cost_micro_usd));

-- name: TenantUsageThisMonth :one
SELECT COALESCE(count(*),0)::bigint AS requests,
  COALESCE(sum(tokens_in),0)::bigint AS tokens_in,
  COALESCE(sum(tokens_out),0)::bigint AS tokens_out
FROM ai_usage_log
WHERE tenant_id = $1 AND created_at >= date_trunc('month', now());

-- name: UserUsageThisMonth :one
SELECT COALESCE(count(*),0)::bigint AS requests,
  COALESCE(sum(tokens_in),0)::bigint AS tokens_in,
  COALESCE(sum(tokens_out),0)::bigint AS tokens_out
FROM ai_usage_log
WHERE user_id = $1 AND created_at >= date_trunc('month', now());

-- name: TenantUsageRollup :many
-- ai:usage_rollup — per-tenant monthly usage for billing/limits.
SELECT tenant_id, count(*)::bigint AS requests,
  COALESCE(sum(tokens_in),0)::bigint AS tokens_in,
  COALESCE(sum(tokens_out),0)::bigint AS tokens_out,
  COALESCE(sum(cost_micro_usd),0)::bigint AS cost_micro_usd
FROM ai_usage_log
WHERE created_at >= date_trunc('month', now())
GROUP BY tenant_id;
