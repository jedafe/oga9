ALTER TABLE organizations DROP COLUMN member_no_seq;
ALTER TABLE organization_memberships DROP COLUMN dues_standing, DROP COLUMN last_renewed_at;
DROP TABLE membership_renewals;
