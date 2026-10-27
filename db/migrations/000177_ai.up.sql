-- Ref: oga9-engineering/sections/22-ai.md

CREATE TABLE ai_conversations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  organization_id uuid REFERENCES organizations(id),
  persona text NOT NULL DEFAULT 'general',           -- general|secretary|treasurer|historian
  created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ai_messages (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  conversation_id uuid NOT NULL REFERENCES ai_conversations(id),
  role text NOT NULL,                                -- user|assistant|system
  body text NOT NULL, citations jsonb,
  tokens_in int, tokens_out int,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_messages_conversation ON ai_messages (conversation_id, created_at);

CREATE TABLE ai_generations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  author_id uuid NOT NULL REFERENCES organization_memberships(id),
  kind text NOT NULL,                                -- agenda|minutes|announcement|report|insight
  draft jsonb NOT NULL,
  state text NOT NULL DEFAULT 'pending_review',      -- pending_review|approved|rejected|applied
  reviewed_by uuid REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_generations_org ON ai_generations (organization_id, state);

CREATE TABLE ai_usage_log (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  user_id uuid REFERENCES users(id),
  kind text NOT NULL, tokens_in int, tokens_out int,
  cost_micro_usd bigint,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_usage_tenant_month ON ai_usage_log (tenant_id, created_at);

-- 'ai-assistant' app: app.ai_assistant capability + platform flag
-- (§22 header) gates activation.
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('ai-assistant','1.0.0','AI Assistant',false,'{
  "id":"ai-assistant","version":"1.0.0","requires":["organization_core","members"],
  "permissions":["org.ai.ask","org.ai.drafts.manage","org.ai.insights.view","platform.ai.usage.view"],
  "events":{"publishes":["AIQueryAnswered","AIDraftGenerated","AIDraftApproved","AIDraftRejected","AIDraftApplied","AIQuotaExceeded"],"consumes":[]},
  "entitlements":{"feature":"app.ai_assistant"}
}');
