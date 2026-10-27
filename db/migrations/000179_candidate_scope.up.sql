-- Ref: oga9-engineering/sections/21-elections.md — scoped contests:
-- a candidate contests position_id within an org_unit scope (branch/
-- committee); the winner's appointment inherits that scope.

ALTER TABLE candidates
  DROP CONSTRAINT candidates_election_id_position_id_membership_id_key,
  ADD COLUMN scope_unit_id uuid REFERENCES org_units(id);

CREATE UNIQUE INDEX candidates_unique_seat ON candidates
  (election_id, position_id, membership_id,
   COALESCE(scope_unit_id, '00000000-0000-0000-0000-000000000000'::uuid));
