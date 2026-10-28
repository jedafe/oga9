UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  ((manifest::jsonb->'permissions')::jsonb -
    '["org.finance.budgets.manage","org.finance.expenses.request","org.finance.expenses.approve","org.finance.expenses.disburse","org.finance.invoices.manage"]'::jsonb))::json
WHERE app_id = 'finance_lite';
DELETE FROM role_permissions WHERE permission IN (
  'org.finance.budgets.manage','org.finance.expenses.request',
  'org.finance.expenses.approve','org.finance.expenses.disburse',
  'org.finance.invoices.manage');
DELETE FROM permissions WHERE node IN (
  'org.finance.budgets.manage','org.finance.expenses.request',
  'org.finance.expenses.approve','org.finance.expenses.disburse',
  'org.finance.invoices.manage');
ALTER TABLE organizations DROP COLUMN invoice_no_seq;
ALTER TABLE ledger_entries DROP COLUMN budget_line_id, DROP COLUMN expense_id;
ALTER TABLE payments DROP COLUMN invoice_id;
DROP TABLE invoices;
DROP TABLE expense_approvals;
DROP TABLE expenses;
DROP TABLE budget_lines;
DROP TABLE budgets;
