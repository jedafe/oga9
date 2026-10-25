-- Ref: oga9-engineering/sections/11-members.md (group 9)
CREATE TABLE member_imports (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  uploaded_by uuid NOT NULL REFERENCES organization_memberships(id),
  filename text NOT NULL, row_count int, ok_count int, err_count int,
  data jsonb,                                 -- parsed CSV rows for the async worker
  errors jsonb, status text NOT NULL DEFAULT 'processing',  -- processing|done|failed
  created_at timestamptz NOT NULL DEFAULT now()
);
