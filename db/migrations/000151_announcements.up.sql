-- Ref: oga9-engineering/sections/14-comms-documents.md — announcements
-- with audience snapshot + notifications inbox (g11, renumbered per
-- MIGRATION-ORDER: reserved 000090–000099 is below the applied max and
-- golang-migrate would silently skip it — next free numbers are used).
CREATE TABLE announcements (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  author_id uuid NOT NULL REFERENCES organization_memberships(id),
  title text NOT NULL, body text NOT NULL,
  audience jsonb NOT NULL DEFAULT '{"all":true}',  -- units/categories/positions
  state text NOT NULL DEFAULT 'published',         -- draft|published|retracted
  published_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX announcements_org ON announcements (organization_id, created_at DESC)
  WHERE state <> 'retracted';
