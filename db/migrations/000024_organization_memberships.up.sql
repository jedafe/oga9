-- Ref: oga9-engineering/sections/04-tenancy-organizations.md (ADR-003/014)
CREATE TABLE organization_memberships (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  user_id uuid NOT NULL REFERENCES users(id),
  member_no text,
  category text NOT NULL DEFAULT 'floor',
  lifecycle_state text NOT NULL DEFAULT 'applied',
  user_group_id uuid REFERENCES user_groups(id),
  joined_at timestamptz, created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, user_id)
);
CREATE INDEX memberships_user ON organization_memberships (user_id);
