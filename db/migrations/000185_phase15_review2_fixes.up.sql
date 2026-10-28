-- Ref: oga9-engineering/sections/23-integrations.md + 20/22 — review fixes.
-- 1) §23: verified japa_subject persists durably on the application row.
-- 2) §20: project_expenses spend reservations make budget/mandate caps
--    race-free — reserved inside the spend tx (project row lock +
--    committee advisory lock), so check-and-reserve is atomic.
-- 3) §22: post-MVP flags ship disabled (feature-flag-naming.md) — seed
--    platform.ai_assistant.enabled=false explicitly; ON CONFLICT keeps
--    an operator-set value.

ALTER TABLE membership_applications
  ADD COLUMN japa_subject text;                     -- verified japa OIDC sub (§23)

CREATE TABLE project_expenses (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  project_id uuid NOT NULL REFERENCES projects(id),
  unit_id uuid REFERENCES org_units(id),            -- committee tag (§16 mandate)
  amount_minor bigint NOT NULL, currency text NOT NULL,
  memo text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX project_expenses_project ON project_expenses (project_id);
CREATE INDEX project_expenses_unit ON project_expenses (organization_id, unit_id)
  WHERE unit_id IS NOT NULL;

INSERT INTO system_settings (key, value)
VALUES ('platform.ai_assistant.enabled', 'false')
ON CONFLICT (key) DO NOTHING;
