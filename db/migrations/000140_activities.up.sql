-- Ref: oga9-engineering/sections/13-events-meetings.md — the shared
-- activity spine; kind discriminates meeting (governance) from event
-- (social), Rule 22. unit_id carries branch/committee scope.
CREATE TABLE activities (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  kind text NOT NULL,                           -- meeting|event
  title text NOT NULL, description text,
  venue text, starts_at timestamptz NOT NULL, ends_at timestamptz,
  unit_id uuid REFERENCES org_units(id),        -- branch/committee scope
  status text NOT NULL DEFAULT 'scheduled',     -- scheduled|completed|cancelled
  quorum_percent int,                           -- meetings only (from org_settings)
  capacity int,                                 -- events only
  created_by uuid NOT NULL REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX activities_org_kind ON activities (organization_id, kind);
-- calendar + remind_upcoming windows scan by start time
CREATE INDEX activities_org_starts ON activities (organization_id, starts_at)
  WHERE status = 'scheduled';
CREATE INDEX activities_unit ON activities (unit_id) WHERE unit_id IS NOT NULL;
