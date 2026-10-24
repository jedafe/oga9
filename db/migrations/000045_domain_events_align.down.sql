ALTER INDEX idx_events_pending RENAME TO domain_events_unpublished;
ALTER TABLE domain_events RENAME COLUMN dispatched_at TO published_at;
ALTER TABLE domain_events RENAME COLUMN event_type TO name;
ALTER TABLE domain_events DROP COLUMN sourced;
ALTER TABLE domain_events DROP COLUMN organization_id;
