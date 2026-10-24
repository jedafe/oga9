-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md
-- Append-only audit trail (Rule 10, ADR-020). organization_id FK is added
-- by a later ALTER once organizations exists (deferred-FK pattern).
CREATE TABLE audit_events (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid REFERENCES tenants(id),
  organization_id uuid,                          -- FK added via later ALTER (deferred-FK)
  actor_id        uuid REFERENCES users(id),
  action          text NOT NULL,                 -- e.g. impersonation.start
  target_type     text,
  target_id       uuid,
  context         jsonb,
  created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX audit_events_actor ON audit_events (actor_id, created_at DESC);
CREATE INDEX audit_events_org ON audit_events (organization_id, created_at DESC) WHERE organization_id IS NOT NULL;
CREATE INDEX audit_events_tenant ON audit_events (tenant_id, created_at DESC) WHERE tenant_id IS NOT NULL;
