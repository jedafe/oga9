-- Ref: oga9-engineering/sections/11-members.md — one ACTIVE card per
-- member; re-issue goes through revoke first (keeps serial history).
DROP INDEX membership_cards_member;
CREATE UNIQUE INDEX membership_cards_member
  ON membership_cards (membership_id) WHERE revoked_at IS NULL;
