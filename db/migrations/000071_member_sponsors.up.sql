-- Ref: oga9-engineering/sections/11-members.md (group 9)
CREATE TABLE member_sponsors (
  membership_id uuid REFERENCES organization_memberships(id),
  sponsor_member_id uuid REFERENCES organization_memberships(id),
  kind text NOT NULL DEFAULT 'sponsor',        -- sponsor|seconder|referrer
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (membership_id, sponsor_member_id)
);
