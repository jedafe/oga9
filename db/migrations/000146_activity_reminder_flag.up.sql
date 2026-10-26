-- Ref: oga9-engineering/sections/13-events-meetings.md — emit-once marker for
-- activities:remind_upcoming. Without it the 5-min tick re-emits
-- MeetingReminderDue for every activity inside its 48h window.
ALTER TABLE activities ADD COLUMN reminder_sent boolean NOT NULL DEFAULT false;
