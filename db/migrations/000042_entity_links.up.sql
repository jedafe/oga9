-- Ref: oga9-engineering/sections/05-domain-events-infrastructure.md (ADR-015)
-- Graph edges between entities for "related" queries — edges are derived
-- from domain events by the entity_links consumer.
CREATE TABLE entity_links (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id),
  source_type text NOT NULL, source_id uuid NOT NULL,
  target_type text NOT NULL, target_id uuid NOT NULL,
  relationship_type text NOT NULL,
  created_by uuid REFERENCES users(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(source_type, source_id, target_type, target_id, relationship_type)
);
CREATE INDEX entity_links_target ON entity_links (target_type, target_id);
CREATE INDEX entity_links_org ON entity_links (organization_id)
  WHERE organization_id IS NOT NULL;
