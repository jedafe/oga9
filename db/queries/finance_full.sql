-- Ref: oga9-engineering/sections/25-finance-full.md — budgets, expenses,
-- invoices. Ledger stays append-only (Rule 19); disbursement posts ledger
-- + payment + spent projection in one tx (Rule 12).

-- ---------- budgets ----------
-- name: CreateBudget :one
INSERT INTO budgets (organization_id, term_id, fiscal_year, name)
VALUES (sqlc.arg(organization_id), sqlc.narg(term_id)::uuid,
  sqlc.arg(fiscal_year), sqlc.arg(name))
RETURNING *;

-- name: GetBudget :one
SELECT * FROM budgets WHERE id = $1 AND organization_id = $2;

-- name: ListBudgets :many
SELECT * FROM budgets WHERE organization_id = $1
ORDER BY fiscal_year DESC, created_at DESC;

-- name: UpdateBudget :one
UPDATE budgets SET
  name = COALESCE(sqlc.narg(name)::text, name),
  fiscal_year = COALESCE(sqlc.narg(fiscal_year)::int, fiscal_year)
WHERE id = sqlc.arg(id)::uuid AND organization_id = sqlc.arg(organization_id)::uuid
  AND state = 'draft'
RETURNING *;

-- name: AdoptBudget :one
-- draft → adopted; term_id binds at adoption (Rule 21) unless already set.
UPDATE budgets SET state = 'adopted', adopted_by = $3, adopted_at = now(),
  term_id = COALESCE(term_id, $4)
WHERE id = $1 AND organization_id = $2 AND state = 'draft'
RETURNING *;

-- name: CloseBudget :execrows
UPDATE budgets SET state = 'closed'
WHERE id = $1 AND organization_id = $2 AND state = 'adopted';

-- name: AddBudgetLine :one
INSERT INTO budget_lines (budget_id, label, allocated_minor, currency)
VALUES (sqlc.arg(budget_id), sqlc.arg(label), sqlc.arg(allocated_minor), sqlc.arg(currency))
RETURNING *;

-- name: UpdateBudgetLine :execrows
UPDATE budget_lines SET
  label = COALESCE(sqlc.narg(label)::text, label),
  allocated_minor = COALESCE(sqlc.narg(allocated_minor)::bigint, allocated_minor)
WHERE id = sqlc.arg(id)::uuid
  AND budget_id IN (SELECT id FROM budgets WHERE id = budget_lines.budget_id AND state = 'draft');

-- name: ListBudgetLines :many
SELECT * FROM budget_lines WHERE budget_id = $1 ORDER BY label;

-- name: GetBudgetLine :one
-- org-scoped through the parent budget.
SELECT bl.* FROM budget_lines bl
JOIN budgets b ON b.id = bl.budget_id
WHERE bl.id = $1 AND b.organization_id = $2;

-- name: BudgetVsActual :many
-- vs-actual read: allocated vs the spent_minor projection (ledger truth
-- maintained at disburse; this re-derives for the report surface).
SELECT bl.id, bl.label, bl.allocated_minor, bl.currency, bl.spent_minor,
  COALESCE((SELECT sum(le.amount_minor) FROM ledger_entries le
    WHERE le.budget_line_id = bl.id AND le.direction = 'debit'
      AND le.reversal_of IS NULL), 0)::bigint AS ledger_spent_minor
FROM budget_lines bl WHERE bl.budget_id = $1 ORDER BY bl.label;

-- name: BumpBudgetLineSpent :one
-- Disbursement projection update — called inside the disburse tx.
UPDATE budget_lines SET spent_minor = spent_minor + $2
WHERE id = $1 RETURNING *;

-- ---------- expenses ----------
-- name: CreateExpense :one
INSERT INTO expenses (organization_id, budget_line_id, mandate_id, title,
  details, payee, amount_minor, currency, requested_by, resolution_id)
VALUES (sqlc.arg(organization_id), sqlc.narg(budget_line_id)::uuid,
  sqlc.narg(mandate_id)::uuid, sqlc.arg(title), sqlc.narg(details)::text,
  sqlc.narg(payee)::text, sqlc.arg(amount_minor), sqlc.arg(currency),
  sqlc.arg(requested_by), sqlc.narg(resolution_id)::uuid)
RETURNING *;

-- name: GetExpense :one
SELECT * FROM expenses WHERE id = $1 AND organization_id = $2;

-- name: ListExpenses :many
SELECT * FROM expenses WHERE organization_id = $1
  AND ($2::text = '' OR state = $2)
ORDER BY created_at DESC LIMIT 500;

-- name: ApproveExpense :one
-- requested → approved; the CAS enforces requester != approver
-- (separation of duties — same membership can't hold two of three roles).
UPDATE expenses SET state = 'approved'
WHERE id = $1 AND organization_id = $2 AND state = 'requested'
  AND requested_by <> $3
RETURNING *;

-- name: RejectExpense :one
UPDATE expenses SET state = 'rejected'
WHERE id = $1 AND organization_id = $2 AND state = 'requested'
  AND requested_by <> $3
RETURNING *;

-- name: CancelExpense :execrows
-- requester may cancel their own expense while undecided.
UPDATE expenses SET state = 'cancelled'
WHERE id = $1 AND organization_id = $2 AND requested_by = $3
  AND state IN ('requested','approved');

-- name: DisburseExpense :one
-- approved → disbursed; disburser must differ from requester (the approver
-- check happens in the service against expense_approvals).
UPDATE expenses SET state = 'disbursed', payment_id = $3
WHERE id = $1 AND organization_id = $2 AND state = 'approved'
  AND requested_by <> $4
RETURNING *;

-- name: AddExpenseApproval :one
-- append-only decision trail.
INSERT INTO expense_approvals (expense_id, approver_id, decision, note)
VALUES (sqlc.arg(expense_id), sqlc.arg(approver_id), sqlc.arg(decision), sqlc.narg(note)::text)
RETURNING *;

-- name: LatestExpenseApproval :one
SELECT * FROM expense_approvals WHERE expense_id = $1 AND decision = 'approved'
ORDER BY decided_at DESC LIMIT 1;

-- name: ListExpenseApprovals :many
SELECT * FROM expense_approvals WHERE expense_id = $1 ORDER BY decided_at;

-- name: ExpenseSpentForMandate :one
-- §16 MandateLimit seam support: committee-linked spend total (pending +
-- approved + disbursed expenses count against the mandate cap).
SELECT COALESCE(sum(amount_minor), 0)::bigint FROM expenses
WHERE mandate_id = $1 AND state IN ('requested','approved','disbursed','reconciled');

-- name: ReconcileExpense :execrows
UPDATE expenses SET state = 'reconciled'
WHERE id = $1 AND organization_id = $2 AND state = 'disbursed';

-- ---------- invoices ----------
-- name: NextInvoiceSeq :one
UPDATE organizations SET invoice_no_seq = invoice_no_seq + 1
WHERE id = $1 RETURNING invoice_no_seq;

-- name: CreateInvoice :one
INSERT INTO invoices (organization_id, membership_id, payee_name,
  payee_contact, number, amount_minor, currency, state, due_on, issued_by)
VALUES (sqlc.arg(organization_id), sqlc.narg(membership_id)::uuid,
  sqlc.narg(payee_name)::text, sqlc.narg(payee_contact)::text,
  sqlc.arg(number), sqlc.arg(amount_minor), sqlc.arg(currency),
  sqlc.arg(state), sqlc.narg(due_on)::date, sqlc.arg(issued_by))
RETURNING *;

-- name: GetInvoice :one
SELECT * FROM invoices WHERE id = $1 AND organization_id = $2;

-- name: ListInvoices :many
SELECT * FROM invoices WHERE organization_id = $1
  AND ($2::text = '' OR state = $2)
ORDER BY created_at DESC LIMIT 500;

-- name: VoidInvoice :one
-- void ≠ delete (Rule 19 spirit) — audited state change.
UPDATE invoices SET state = 'void', voided_by = $3, void_reason = $4
WHERE id = $1 AND organization_id = $2 AND state IN ('draft','issued','partially_paid')
RETURNING *;

-- name: InvoicePaidTotal :one
-- state derives from confirmed ledger money only.
SELECT COALESCE(sum(amount_minor), 0)::bigint FROM payments
WHERE invoice_id = $1 AND status IN ('confirmed','reconciled');

-- name: SetInvoiceState :execrows
UPDATE invoices SET state = $3
WHERE id = $1 AND organization_id = $2 AND state <> 'void';

-- name: InvoicePayments :many
SELECT * FROM payments WHERE invoice_id = $1
ORDER BY created_at DESC;

-- name: PaymentsForInvoiceTotal :one
SELECT count(*)::int FROM payments WHERE invoice_id = $1
  AND status IN ('confirmed','reconciled');

-- name: GetInvoiceAny :one
-- Task/refresh context — org read from the row.
SELECT * FROM invoices WHERE id = $1;

-- name: ConfirmSystemPayment :one
-- system-recorded payments (expense disbursement, invoice settlement)
-- confirm without the recorder!=approver CAS — separation of duties for
-- these lives on the expense/invoice workflow, not the payment row.
UPDATE payments SET status = 'confirmed', approved_by = $3, confirmed_at = now()
WHERE id = $1 AND organization_id = $2 AND status = 'initiated'
RETURNING *;

-- name: DisbursedExpenseForPayment :one
-- §25: a payment backing a disbursed expense cannot be reversed — the
-- expense itself must be voided/re-done, else the budget projection orphans.
SELECT id, state FROM expenses WHERE payment_id = $1 LIMIT 1;
