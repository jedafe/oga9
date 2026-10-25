-- Ref: oga9-engineering/sections/11-members.md (group 9)
CREATE TABLE membership_applications (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  user_id uuid REFERENCES users(id),          -- null = pre-signup application
  email citext, phone text,
  answers jsonb NOT NULL DEFAULT '{}',         -- org-configurable fields
  sponsor_member_id uuid REFERENCES organization_memberships(id),
  status text NOT NULL DEFAULT 'applied',      -- applied|screened|approved|rejected|withdrawn
  screened_by uuid REFERENCES organization_memberships(id),
  decided_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX membership_apps_org ON membership_applications (organization_id, status);
