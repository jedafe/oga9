-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
-- DLQ re-drive cap: retry_dead increments redrive_count per cycle; events
-- past the cap stay dead so permanently-poison payloads can't loop forever.
ALTER TABLE domain_events ADD COLUMN redrive_count int NOT NULL DEFAULT 0;
