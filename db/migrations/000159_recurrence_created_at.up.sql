-- Ref: oga9-engineering/sections/13-events-meetings.md — RRULE INTERVAL
-- anchors on the series start (registration time), not whatever day the
-- sweep ran: INTERVAL=2 yearly must mean "every other year FROM the
-- series", not "from today".
ALTER TABLE activity_recurrences
  ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
