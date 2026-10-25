DROP VIEW membership_history;
DROP INDEX member_sponsors_application_uq, member_sponsors_membership_uq;
ALTER TABLE member_sponsors
  DROP CONSTRAINT member_sponsors_target_chk,
  ALTER COLUMN membership_id SET NOT NULL,
  DROP COLUMN application_id,
  DROP COLUMN id;
ALTER TABLE member_sponsors ADD PRIMARY KEY (membership_id, sponsor_member_id);
ALTER TABLE organization_memberships DROP COLUMN org_unit_id;
