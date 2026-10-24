-- Deferred-FK: audit_events.organization_id -> organizations (group 6 note,
-- audit_events ships before organizations exists in early-phase trees).
ALTER TABLE audit_events ADD CONSTRAINT audit_events_org_fk
  FOREIGN KEY (organization_id) REFERENCES organizations(id);
