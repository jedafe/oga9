-- Ref: oga9-engineering/sections/13-events-meetings.md — a guest contact
-- can only be registered once per event (member rows already dedupe via
-- the partial unique on membership_id).
CREATE UNIQUE INDEX event_registrations_guest_contact
  ON event_registrations (activity_id, lower(guest_contact))
  WHERE guest_contact IS NOT NULL;
