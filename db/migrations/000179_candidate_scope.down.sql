DROP INDEX candidates_unique_seat;
ALTER TABLE candidates
  DROP COLUMN scope_unit_id,
  ADD CONSTRAINT candidates_election_id_position_id_membership_id_key
    UNIQUE(election_id, position_id, membership_id);
