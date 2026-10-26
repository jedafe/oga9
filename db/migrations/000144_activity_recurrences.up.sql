-- Ref: oga9-engineering/sections/13-events-meetings.md — RRULE templates
-- evaluated by activities:generate_recurrences into concrete activities.
CREATE TABLE activity_recurrences (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  kind text NOT NULL, template jsonb NOT NULL,  -- title/venue/duration defaults
  rrule text NOT NULL,                          -- RRULE string
  active boolean NOT NULL DEFAULT true
);
CREATE INDEX activity_recurrences_org ON activity_recurrences (organization_id)
  WHERE active;
