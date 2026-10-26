-- Ref: oga9-engineering/sections/12-finance-lite.md — contribution taxonomy
-- (org-extensible) + dues/levy schedules that drive obligation generation.
CREATE TABLE contribution_types (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  key text NOT NULL,                          -- dues|registration|levy|donation|welfare|project|fine|custom:*
  label text NOT NULL, kind text NOT NULL DEFAULT 'assessed', -- assessed|voluntary|penalty
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, key)
);
CREATE INDEX contribution_types_org ON contribution_types (organization_id);

CREATE TABLE dues_schedules (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  contribution_type_id uuid NOT NULL REFERENCES contribution_types(id),
  name text NOT NULL,                          -- 'Monthly dues 2026'
  amount_minor bigint NOT NULL, currency text NOT NULL,
  recurrence text NOT NULL,                    -- monthly|quarterly|annual|once
  applies_to jsonb NOT NULL DEFAULT '{}',      -- {category:[...],unit_ids:[...]} filter
  starts_on date NOT NULL, ends_on date,
  active boolean NOT NULL DEFAULT true
);
CREATE INDEX dues_schedules_org_active ON dues_schedules (organization_id) WHERE active;
