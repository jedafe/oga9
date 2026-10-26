-- Ref: oga9-engineering/sections/13-events-meetings.md — agenda is a single
-- JSONB item list per meeting; minutes are versioned, immutable on approve.
CREATE TABLE meeting_agendas (
  activity_id uuid PRIMARY KEY REFERENCES activities(id),
  items jsonb NOT NULL DEFAULT '[]'             -- [{order,title,mover,duration}]
);

CREATE TABLE minutes (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  activity_id uuid NOT NULL REFERENCES activities(id),
  body text NOT NULL, version int NOT NULL DEFAULT 1,
  state text NOT NULL DEFAULT 'draft',          -- draft|approved|archived
  approved_by uuid REFERENCES organization_memberships(id),
  approved_at timestamptz,
  UNIQUE(activity_id, version)
);
CREATE INDEX minutes_activity ON minutes (activity_id);
