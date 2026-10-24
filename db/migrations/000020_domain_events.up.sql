-- Group 6 (pulled forward for Phase 02 event emission): domain-event outbox.
-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
CREATE TABLE domain_events (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name          text NOT NULL,               -- e.g. user.registered
  aggregate_type text NOT NULL,              -- e.g. user, organization, payment
  aggregate_id  uuid NOT NULL,
  tenant_id     uuid REFERENCES tenants(id), -- null = platform-scoped event
  actor_id      uuid REFERENCES users(id),   -- who triggered it (nullable for system)
  payload       jsonb NOT NULL DEFAULT '{}',
  occurred_at   timestamptz NOT NULL DEFAULT now(),
  published_at  timestamptz,                 -- set when outbox dispatcher delivers
  attempts      int NOT NULL DEFAULT 0,
  last_error    text
);
CREATE INDEX domain_events_unpublished ON domain_events (occurred_at) WHERE published_at IS NULL;
CREATE INDEX domain_events_agg ON domain_events (aggregate_type, aggregate_id);
