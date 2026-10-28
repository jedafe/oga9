-- Ref: oga9-engineering/sections/20-projects.md — project budgets: the
-- projects domain owns budget lines; spend posts to the finance ledger
-- via ProjectExpenseRecorded (Rule 26 — never a shared table).
CREATE TABLE project_budgets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  project_id uuid NOT NULL UNIQUE REFERENCES projects(id), -- one budget per project
  amount_minor bigint NOT NULL, currency text NOT NULL,
  note text, created_by uuid REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE projects ADD CONSTRAINT projects_budget_fk
  FOREIGN KEY (budget_id) REFERENCES project_budgets(id);
