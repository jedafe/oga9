-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
-- Consumer idempotency ledger (Rule 27): a (consumer, event) pair is
-- recorded in the same tx as the consumer's own writes — at-least-once
-- delivery can never double-apply.
CREATE TABLE event_consumer_log (
  consumer text NOT NULL,
  event_id uuid NOT NULL REFERENCES domain_events(id),
  processed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (consumer, event_id)
);
