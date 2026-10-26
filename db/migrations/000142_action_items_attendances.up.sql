-- Ref: oga9-engineering/sections/13-events-meetings.md — action items and
-- the attendance sheet. Action-item overdue sweep scans state+due_date;
-- attendance is a composite-key mark sheet (bulk upsert surface).
CREATE TABLE action_items (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  activity_id uuid NOT NULL REFERENCES activities(id),
  assignee_id uuid NOT NULL REFERENCES organization_memberships(id),
  title text NOT NULL, due_date date,
  state text NOT NULL DEFAULT 'open',           -- open|done|overdue|cancelled
  done_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX action_items_activity ON action_items (activity_id);
CREATE INDEX action_items_assignee ON action_items (assignee_id, state);
CREATE INDEX action_items_due ON action_items (due_date)
  WHERE state IN ('open');

CREATE TABLE attendances (
  activity_id uuid NOT NULL REFERENCES activities(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  status text NOT NULL DEFAULT 'absent',        -- present|absent|excused|late
  checked_at timestamptz, note text,
  PRIMARY KEY (activity_id, membership_id)
);
CREATE INDEX attendances_member ON attendances (membership_id);
