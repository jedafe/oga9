-- Ref: oga9-engineering/sections/11-members.md (group 9 tail)
-- 1) Member home-unit scope: drives the directory `unit` filter.
ALTER TABLE organization_memberships
  ADD COLUMN org_unit_id uuid REFERENCES org_units(id);
CREATE INDEX memberships_unit ON organization_memberships (org_unit_id) WHERE org_unit_id IS NOT NULL;

-- 2) Application-stage sponsors: member_sponsors can point at an
-- application before a membership exists (sponsor_required up to 2).
ALTER TABLE member_sponsors
  DROP CONSTRAINT member_sponsors_pkey,
  ADD COLUMN id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  ADD COLUMN application_id uuid REFERENCES membership_applications(id),
  ALTER COLUMN membership_id DROP NOT NULL,
  ADD CONSTRAINT member_sponsors_target_chk
    CHECK (membership_id IS NOT NULL OR application_id IS NOT NULL);
CREATE UNIQUE INDEX member_sponsors_membership_uq
  ON member_sponsors (membership_id, sponsor_member_id) WHERE membership_id IS NOT NULL;
CREATE UNIQUE INDEX member_sponsors_application_uq
  ON member_sponsors (application_id, sponsor_member_id) WHERE application_id IS NOT NULL;

-- 3) membership_history view: the sourced event stream over membership-
-- family aggregates (§11).
CREATE VIEW membership_history AS
  SELECT * FROM domain_events
  WHERE sourced
    AND aggregate_type IN ('organization_membership', 'membership_application',
                           'membership_card', 'membership_renewal', 'member_import');
