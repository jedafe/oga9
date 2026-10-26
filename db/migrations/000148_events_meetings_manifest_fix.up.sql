-- Ref: oga9-engineering/sections/13-events-meetings.md — manifest drift:
-- events_meetings declared 4 of the 7 §13 nodes. Meetings manage,
-- attendance record and action-item manage were missing (same drift
-- class 000107/000108 fixed for the other manifests).
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.activities.view","org.meetings.manage","org.meetings.attendance.record","org.minutes.approve","org.action_items.manage","org.events.manage","org.events.register_own"]')
WHERE app_id = 'events_meetings';
