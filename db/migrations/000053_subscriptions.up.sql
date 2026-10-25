-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
CREATE TABLE subscriptions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  plan_slug text NOT NULL REFERENCES plans(slug),
  state text NOT NULL DEFAULT 'trial',          -- trial|active|past_due|grace|restricted|cancelled
  billing_term text NOT NULL DEFAULT 'monthly', -- monthly|annual|enterprise_contract
  trial_ends_at timestamptz, current_period_end timestamptz,
  grace_ends_at timestamptz, cancelled_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX subscriptions_tenant ON subscriptions (tenant_id, created_at DESC);
