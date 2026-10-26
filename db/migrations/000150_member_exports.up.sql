-- Ref: oga9-engineering/sections/11-members.md — durable export row for
-- membership:export (mirrors member_imports; content lands when the
-- worker finishes). Blob storage replaces content with a key later.
CREATE TABLE member_exports (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  requested_by uuid NOT NULL REFERENCES organization_memberships(id),
  filename text NOT NULL,
  content bytea,
  row_count int,
  status text NOT NULL DEFAULT 'processing',  -- processing|done|failed
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX member_exports_org ON member_exports (organization_id, created_at DESC);
