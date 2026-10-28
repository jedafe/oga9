-- Ref: oga9-engineering/sections/25-finance-full.md (group 23) — budgets,
-- expense request→approve→disburse, org-issued invoices. Ledger stays
-- append-only (Rule 19); budgets are term-scoped (Rule 21).
-- Requires g12 (payments/ledger_entries) + g15 (committee_mandates,
-- resolutions).

CREATE TABLE budgets (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  term_id uuid REFERENCES terms(id),          -- adopted under a term (Rule 21)
  fiscal_year int NOT NULL,
  name text NOT NULL,                          -- e.g. 'FY2027 General'
  state text NOT NULL DEFAULT 'draft',         -- draft|adopted|closed
  adopted_by uuid REFERENCES organization_memberships(id),
  adopted_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, fiscal_year, name)
);
CREATE INDEX budgets_org ON budgets (organization_id, fiscal_year);

CREATE TABLE budget_lines (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  budget_id uuid NOT NULL REFERENCES budgets(id) ON DELETE CASCADE,
  label text NOT NULL,                         -- dues|welfare|projects|operations|events|custom
  allocated_minor bigint NOT NULL,
  spent_minor bigint NOT NULL DEFAULT 0,       -- maintained projection over ledger_entries; ledger is truth
  currency text NOT NULL
);
CREATE INDEX budget_lines_budget ON budget_lines (budget_id);

CREATE TABLE expenses (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  budget_line_id uuid REFERENCES budget_lines(id),
  mandate_id uuid REFERENCES committee_mandates(org_unit_id), -- committee-linked spend (§16 MandateLimit seam)
  title text NOT NULL, details text,
  payee text,                                  -- vendor/payee label
  amount_minor bigint NOT NULL, currency text NOT NULL,
  state text NOT NULL DEFAULT 'requested',     -- requested|approved|rejected|disbursed|reconciled|cancelled
  requested_by uuid NOT NULL REFERENCES organization_memberships(id),
  resolution_id uuid REFERENCES resolutions(id),   -- when spend was approved by resolution (§16)
  payment_id uuid REFERENCES payments(id),     -- linked on disburse (payments is g12, before g23)
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX expenses_org ON expenses (organization_id, state, created_at DESC);

CREATE TABLE expense_approvals (               -- append-only decision trail
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  expense_id uuid NOT NULL REFERENCES expenses(id),
  approver_id uuid NOT NULL REFERENCES organization_memberships(id),
  decision text NOT NULL,                      -- approved|rejected
  note text,
  decided_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX expense_approvals_expense ON expense_approvals (expense_id);

CREATE TABLE invoices (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  membership_id uuid REFERENCES organization_memberships(id), -- null = external payee (see payee_*)
  payee_name text, payee_contact text,
  number text NOT NULL,                        -- org-scoped sequence (INV-2027-001)
  amount_minor bigint NOT NULL, currency text NOT NULL,
  state text NOT NULL DEFAULT 'issued',        -- draft|issued|partially_paid|paid|void
  due_on date,
  issued_by uuid REFERENCES organization_memberships(id),
  voided_by uuid REFERENCES organization_memberships(id),
  void_reason text,
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, number)
);
CREATE INDEX invoices_org ON invoices (organization_id, state, created_at DESC);
-- org-scoped invoice sequence (INV-YYYY-NNNNN), same shape as receipt_no_seq.
ALTER TABLE organizations ADD COLUMN invoice_no_seq int NOT NULL DEFAULT 0;
-- §25: invoices settle through payments — the payment carries the link so
-- invoice state derives from confirmed ledger money (same shape as
-- obligation_id/drive_id on the payments row).
ALTER TABLE payments ADD COLUMN invoice_id uuid REFERENCES invoices(id);
CREATE INDEX payments_invoice ON payments (invoice_id);

ALTER TABLE ledger_entries ADD COLUMN budget_line_id uuid REFERENCES budget_lines(id);
ALTER TABLE ledger_entries ADD COLUMN expense_id uuid REFERENCES expenses(id);
CREATE INDEX ledger_entries_expense ON ledger_entries (expense_id);
CREATE INDEX ledger_entries_budget_line ON ledger_entries (budget_line_id);

-- §25 permission nodes (permission-key-naming.md); org_admin inherits via
-- the org.% pattern — scoped grants re-run here so migrated DBs catch up.
INSERT INTO permissions (node, label) VALUES
('org.finance.budgets.manage','Manage budgets'),
('org.finance.expenses.request','Request expenses'),
('org.finance.expenses.approve','Approve expenses'),
('org.finance.expenses.disburse','Disburse expenses'),
('org.finance.invoices.manage','Manage invoices');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'org_admin' AND r.tenant_id IS NULL AND p.node IN (
  'org.finance.budgets.manage','org.finance.expenses.request',
  'org.finance.expenses.approve','org.finance.expenses.disburse',
  'org.finance.invoices.manage');
-- catalog grants: budgets+invoices → treasurer/financial secretary,
-- approve → treasurer/president, disburse → treasurer, request → officers.
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug IN ('treasurer','financial_secretary') AND r.tenant_id IS NULL
  AND p.node IN ('org.finance.budgets.manage','org.finance.invoices.manage');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug IN ('treasurer','president') AND r.tenant_id IS NULL
  AND p.node = 'org.finance.expenses.approve';
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug IN ('treasurer','financial_secretary','president','secretary')
  AND r.tenant_id IS NULL AND p.node = 'org.finance.expenses.request';
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'treasurer' AND r.tenant_id IS NULL
  AND p.node = 'org.finance.expenses.disburse';

-- extend the finance_lite manifest with the §25 surface (app unchanged —
-- full accounting unlocks on install, §25 header).
UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  (manifest::jsonb->'permissions') ||
  '["org.finance.budgets.manage","org.finance.expenses.request","org.finance.expenses.approve","org.finance.expenses.disburse","org.finance.invoices.manage"]'::jsonb)::json
WHERE app_id = 'finance_lite';
