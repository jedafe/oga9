-- Ref: oga9-engineering/sections/13-events-meetings.md — guest cap
-- enforcement needs to know which member registered each guest;
-- member rows carry membership_id, guest rows carry registered_by.
ALTER TABLE event_registrations
  ADD COLUMN registered_by uuid REFERENCES organization_memberships(id);
CREATE INDEX event_registrations_by ON event_registrations (activity_id, registered_by)
  WHERE registered_by IS NOT NULL;
