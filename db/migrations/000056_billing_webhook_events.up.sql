-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
-- Durable raw record for Rule 9: endpoint inserts (replay-guarded), worker
-- processes async and marks processed_at.
CREATE TABLE billing_webhook_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  provider text NOT NULL, event_id text NOT NULL,
  event_type text NOT NULL, payload jsonb NOT NULL,
  processed_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(provider, event_id)
);
