-- Ref: oga9-engineering/sections/13-events-meetings.md — registration
-- roster. membership_id NULL = guest (name/contact carried on the row).
-- Members register once per event — the partial unique dedupes member
-- rows; guests have no dedupe (same name may legitimately recur).
CREATE TABLE event_registrations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(), -- events reference registration_id
  activity_id uuid NOT NULL REFERENCES activities(id),
  membership_id uuid REFERENCES organization_memberships(id),  -- null=guest
  guest_name text, guest_contact text,
  status text NOT NULL DEFAULT 'registered',    -- registered|checked_in|cancelled|no_show
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX event_registrations_activity ON event_registrations (activity_id);
CREATE UNIQUE INDEX event_registrations_member_uq
  ON event_registrations (activity_id, membership_id)
  WHERE membership_id IS NOT NULL;
