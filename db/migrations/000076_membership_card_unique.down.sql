DROP INDEX membership_cards_member;
CREATE INDEX membership_cards_member
  ON membership_cards (membership_id) WHERE revoked_at IS NULL;
