-- Ref: oga9-engineering/sections/10-organization-core.md (group 8)
CREATE TABLE org_invitations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  email citext, phone text,                   -- one required
  member_no_hint text,
  category text NOT NULL DEFAULT 'floor',
  invited_by uuid NOT NULL REFERENCES organization_memberships(id),
  token uuid NOT NULL UNIQUE,
  status text NOT NULL DEFAULT 'pending',      -- pending|accepted|expired|revoked
  expires_at timestamptz NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX org_invitations_org_pending ON org_invitations (organization_id) WHERE status = 'pending';
