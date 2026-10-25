-- Ref: oga9-engineering/sections/11-members.md (group 9)
CREATE TABLE membership_cards (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  serial text NOT NULL UNIQUE,
  design jsonb NOT NULL DEFAULT '{}',
  issued_at timestamptz NOT NULL DEFAULT now(),
  revoked_at timestamptz
);
CREATE INDEX membership_cards_member ON membership_cards (membership_id) WHERE revoked_at IS NULL;
