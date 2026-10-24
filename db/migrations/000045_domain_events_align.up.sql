-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
-- Align domain_events with the §05 contract before the Phase 05 dispatcher
-- lands: organization_id column (org-scoped queries/projections), sourced
-- flag (ADR-004 selective event sourcing), spec column names.
ALTER TABLE domain_events ADD COLUMN organization_id uuid REFERENCES organizations(id);
ALTER TABLE domain_events ADD COLUMN sourced boolean NOT NULL DEFAULT false;
ALTER TABLE domain_events RENAME COLUMN name TO event_type;
ALTER TABLE domain_events RENAME COLUMN published_at TO dispatched_at;
ALTER INDEX domain_events_unpublished RENAME TO idx_events_pending;

-- Backfill organization_id from the payload keys emitters used.
UPDATE domain_events SET organization_id = (payload->>'organization_id')::uuid
  WHERE organization_id IS NULL AND payload ? 'organization_id';
UPDATE domain_events SET organization_id = (payload->>'org_id')::uuid
  WHERE organization_id IS NULL AND payload ? 'org_id';
