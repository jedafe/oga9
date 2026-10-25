-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
-- Dispatcher backoff: failed consumer passes retry after a per-attempt delay
-- (10s * 2^attempts, capped 1h — "retry w/ backoff → DLQ" contract) instead
-- of burning a dispatch tick each cycle.
ALTER TABLE domain_events ADD COLUMN next_attempt_at timestamptz;
