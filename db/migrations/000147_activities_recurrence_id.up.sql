-- Ref: oga9-engineering/sections/13-events-meetings.md — generated instances
-- carry a real recurrence_id (the rec:-tag-in-description dedupe is replaced
-- by an atomic (recurrence_id, starts_at) unique).
ALTER TABLE activities ADD COLUMN recurrence_id uuid REFERENCES activity_recurrences(id);
CREATE UNIQUE INDEX activities_recurrence_instance
  ON activities (recurrence_id, starts_at) WHERE recurrence_id IS NOT NULL;
