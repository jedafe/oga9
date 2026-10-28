// Ref: oga9-engineering/sections/25-finance-full.md — budgets, expense
// request→approve→disburse (three-role separation, requester ≠ approver ≠
// disburser), org-issued invoices settling through payments only.
package finance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrBudgetOverrun = errors.New("finance: budget_hard_block")
	ErrSeparationExp = errors.New("finance: separation_of_duties")
)

// mandateLimit is the §16 governance seam — wired from main (Rule 26:
// finance never reads committee_mandates itself).
type mandateLimitFn func(ctx context.Context, unitID uuid.UUID) (int64, string, bool)

func derefText(s *string) pgtype.Text {
	if s == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *s, Valid: true}
}
func derefInt(n *int32) pgtype.Int4 {
	if n == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: *n, Valid: true}
}
func pgInt8(n int64) pgtype.Int8 { return pgtype.Int8{Int64: n, Valid: n > 0} }

func (s *Service) SetMandateLimit(fn mandateLimitFn) { s.mandateLimit = fn }

// ---------- budgets ----------

type BudgetIn struct {
	FiscalYear int       `json:"fiscal_year"`
	Name       string    `json:"name"`
	TermID     uuid.UUID `json:"term_id"`
	Lines      []struct {
		Label          string `json:"label"`
		AllocatedMinor int64  `json:"allocated_minor"`
		Currency       string `json:"currency"`
	} `json:"lines"`
}

func (s *Service) CreateBudget(ctx context.Context, oc *tenant.OrgCtx, in BudgetIn) (uuid.UUID, error) {
	if in.FiscalYear < 2000 || in.Name == "" {
		return uuid.Nil, ErrBadValue
	}
	var row db.Budgets
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateBudget(ctx, db.CreateBudgetParams{
			OrganizationID: toPg(oc.OrgID), TermID: toPg(in.TermID),
			FiscalYear: int32(in.FiscalYear), Name: in.Name})
		if err != nil {
			return err
		}
		for _, l := range in.Lines {
			if l.AllocatedMinor < 0 || l.Currency == "" || l.Label == "" {
				return ErrBadValue
			}
			if _, err := q.AddBudgetLine(ctx, db.AddBudgetLineParams{
				BudgetID: row.ID, Label: l.Label,
				AllocatedMinor: l.AllocatedMinor, Currency: l.Currency}); err != nil {
				return err
			}
		}
		return nil
	})
	return fromPg(row.ID), err
}

func (s *Service) ListBudgets(ctx context.Context, oc *tenant.OrgCtx) ([]db.Budgets, error) {
	return s.q.ListBudgets(ctx, toPg(oc.OrgID))
}

func (s *Service) GetBudget(ctx context.Context, oc *tenant.OrgCtx, bid uuid.UUID) (*db.Budgets, []db.BudgetLines, error) {
	b, err := s.q.GetBudget(ctx, db.GetBudgetParams{ID: toPg(bid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, nil, ErrNotFound
	}
	lines, err := s.q.ListBudgetLines(ctx, b.ID)
	return &b, lines, err
}

func (s *Service) PatchBudget(ctx context.Context, oc *tenant.OrgCtx, bid uuid.UUID, name *string, fy *int32, lines []struct {
	ID             uuid.UUID `json:"id"`
	Label          string    `json:"label"`
	AllocatedMinor int64     `json:"allocated_minor"`
	Currency       string    `json:"currency"`
}) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		b, err := q.UpdateBudget(ctx, db.UpdateBudgetParams{
			ID: toPg(bid), OrganizationID: toPg(oc.OrgID), Name: derefText(name), FiscalYear: derefInt(fy)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition // not draft or not this org's
		}
		if err != nil {
			return err
		}
		if lines != nil {
			for _, l := range lines {
				if l.ID != uuid.Nil {
					// draft-only edit of an existing line (label/allocation);
					// currency is locked once set — ledger rows hang off it.
					n, err := q.UpdateBudgetLine(ctx, db.UpdateBudgetLineParams{
						ID: toPg(l.ID), Label: pgText(l.Label), AllocatedMinor: pgInt8(l.AllocatedMinor)})
					if err != nil {
						return err
					}
					if n == 0 {
						return ErrNotFound
					}
					continue
				}
				if _, err := q.AddBudgetLine(ctx, db.AddBudgetLineParams{
					BudgetID: b.ID, Label: l.Label,
					AllocatedMinor: l.AllocatedMinor, Currency: l.Currency}); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func (s *Service) AdoptBudget(ctx context.Context, oc *tenant.OrgCtx, bid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		termID, _ := q.CurrentTermID(ctx, toPg(oc.OrgID))
		if !termID.Valid {
			if b, err := q.GetBudget(ctx, db.GetBudgetParams{ID: toPg(bid), OrganizationID: toPg(oc.OrgID)}); err != nil || !b.TermID.Valid {
				return ErrBadValue // §25: adoption binds a term — none open
			}
		}
		b, err := q.AdoptBudget(ctx, db.AdoptBudgetParams{
			ID: toPg(bid), OrganizationID: toPg(oc.OrgID),
			AdoptedBy: toPg(oc.MembershipID), TermID: termID})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "BudgetAdopted",
			"budget", bid, oc.UserID, true,
			map[string]any{"budget_id": bid, "fy": b.FiscalYear})
	})
}

func (s *Service) CloseBudget(ctx context.Context, oc *tenant.OrgCtx, bid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.CloseBudget(ctx, db.CloseBudgetParams{ID: toPg(bid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrInvalidTransition
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "BudgetClosed",
			"budget", bid, oc.UserID, false, map[string]any{"budget_id": bid})
	})
}

func (s *Service) VsActual(ctx context.Context, oc *tenant.OrgCtx, bid uuid.UUID) ([]db.BudgetVsActualRow, error) {
	if _, err := s.q.GetBudget(ctx, db.GetBudgetParams{ID: toPg(bid), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return nil, ErrNotFound
	}
	return s.q.BudgetVsActual(ctx, toPg(bid))
}

// ---------- expenses ----------

type ExpenseIn struct {
	BudgetLineID uuid.UUID `json:"budget_line_id"`
	MandateID    uuid.UUID `json:"mandate_id"`
	ResolutionID uuid.UUID `json:"resolution_id"`
	Title        string    `json:"title"`
	Details      string    `json:"details"`
	Payee        string    `json:"payee"`
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
}

func (s *Service) RequestExpense(ctx context.Context, oc *tenant.OrgCtx, in ExpenseIn) (uuid.UUID, error) {
	if in.AmountMinor <= 0 || in.Currency == "" || in.Title == "" {
		return uuid.Nil, ErrBadValue
	}
	if in.BudgetLineID != uuid.Nil {
		if _, err := s.q.GetBudgetLine(ctx, db.GetBudgetLineParams{
			ID: toPg(in.BudgetLineID), OrganizationID: toPg(oc.OrgID)}); err != nil {
			return uuid.Nil, ErrNotFound
		}
	}
	if in.MandateID != uuid.Nil && s.mandateLimit != nil {
		// §16 seam: block committee spend already over the mandate cap.
		if lim, _, ok := s.mandateLimit(ctx, in.MandateID); ok {
			spent, _ := s.q.ExpenseSpentForMandate(ctx, toPg(in.MandateID))
			if spent+in.AmountMinor > lim {
				return uuid.Nil, ErrBudgetOverrun
			}
		}
	}
	var row db.Expenses
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateExpense(ctx, db.CreateExpenseParams{
			OrganizationID: toPg(oc.OrgID), BudgetLineID: toPg(in.BudgetLineID),
			MandateID: toPg(in.MandateID), Title: in.Title,
			Details: pgText(in.Details), Payee: pgText(in.Payee),
			AmountMinor: in.AmountMinor, Currency: in.Currency,
			RequestedBy: toPg(oc.MembershipID), ResolutionID: toPg(in.ResolutionID)})
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ExpenseRequested",
			"expense", fromPg(row.ID), oc.UserID, false,
			map[string]any{"expense_id": fromPg(row.ID), "amount_minor": row.AmountMinor})
	})
	return fromPg(row.ID), err
}

func (s *Service) ListExpenses(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.Expenses, error) {
	return s.q.ListExpenses(ctx, db.ListExpensesParams{OrganizationID: toPg(oc.OrgID), Column2: state})
}

// GetExpense — reports.read callers see all; a requester sees their own
// (_own scope — the handler passes through and the service enforces).
func (s *Service) GetExpense(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) (*db.Expenses, []db.ExpenseApprovals, error) {
	e, err := s.q.GetExpense(ctx, db.GetExpenseParams{ID: toPg(eid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, nil, ErrNotFound
	}
	appr, err := s.q.ListExpenseApprovals(ctx, e.ID)
	return &e, appr, err
}

// decideExpense runs approve|reject through one path: append-only
// approval row + CAS transition + event, all in one tx (Rule 12).
func (s *Service) decideExpense(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID, approve bool, note string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		var row db.Expenses
		var err error
		if approve {
			row, err = q.ApproveExpense(ctx, db.ApproveExpenseParams{
				ID: toPg(eid), OrganizationID: toPg(oc.OrgID), RequestedBy: toPg(oc.MembershipID)})
		} else {
			row, err = q.RejectExpense(ctx, db.RejectExpenseParams{
				ID: toPg(eid), OrganizationID: toPg(oc.OrgID), RequestedBy: toPg(oc.MembershipID)})
		}
		if errors.Is(err, pgx.ErrNoRows) {
			cur, e2 := q.GetExpense(ctx, db.GetExpenseParams{ID: toPg(eid), OrganizationID: toPg(oc.OrgID)})
			if e2 != nil {
				return ErrNotFound
			}
			if fromPg(cur.RequestedBy) == oc.MembershipID {
				return ErrSeparationExp // requester can't decide their own expense
			}
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		dec := "approved"
		if !approve {
			dec = "rejected"
		}
		if _, err := q.AddExpenseApproval(ctx, db.AddExpenseApprovalParams{
			ExpenseID: row.ID, ApproverID: toPg(oc.MembershipID),
			Decision: dec, Note: pgText(note)}); err != nil {
			return err
		}
		ev := "ExpenseApproved"
		if !approve {
			ev = "ExpenseRejected"
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev,
			"expense", fromPg(row.ID), oc.UserID, true,
			map[string]any{"expense_id": fromPg(row.ID), "amount_minor": row.AmountMinor,
				"approver_id": oc.MembershipID, "note": note})
	})
}

func (s *Service) ApproveExpense(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID, note string) error {
	return s.decideExpense(ctx, oc, eid, true, note)
}
func (s *Service) RejectExpense(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID, note string) error {
	return s.decideExpense(ctx, oc, eid, false, note)
}

func (s *Service) CancelExpense(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.CancelExpense(ctx, db.CancelExpenseParams{
			ID: toPg(eid), OrganizationID: toPg(oc.OrgID), RequestedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrInvalidTransition
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ExpenseCancelled",
			"expense", eid, oc.UserID, false,
			map[string]any{"expense_id": eid, "by": oc.MembershipID})
	})
}

// DisburseExpense — approved → disbursed: payment row, ledger debit tagged
// expense+budget_line, spent projection, receipt, and the event — one tx.
// Budget overrun warns via BudgetLineExceeded unless the org rule
// org.finance.budget.hard_block rejects it (Rule 23, not hardcoded).
func (s *Service) DisburseExpense(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID, channel, memo string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		e, err := q.GetExpense(ctx, db.GetExpenseParams{ID: toPg(eid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		if fromPg(e.RequestedBy) == oc.MembershipID {
			return ErrSeparationExp
		}
		if ap, err := q.LatestExpenseApproval(ctx, e.ID); err == nil &&
			fromPg(ap.ApproverID) == oc.MembershipID {
			return ErrSeparationExp // approver can't also disburse
		}
		// §16 seam: the disburse check enforces committee_mandates cap.
		if e.MandateID.Valid && s.mandateLimit != nil {
			if lim, _, ok := s.mandateLimit(ctx, fromPg(e.MandateID)); ok {
				spent, _ := q.ExpenseSpentForMandate(ctx, e.MandateID)
				if spent+e.AmountMinor > lim {
					return ErrBudgetOverrun
				}
			}
		}
		if !paymentChannels[channel] {
			return ErrBadChannel
		}
		// budget overrun check (soft warn vs hard block — org rule).
		if e.BudgetLineID.Valid {
			bl, err := q.GetBudgetLine(ctx, db.GetBudgetLineParams{
				ID: e.BudgetLineID, OrganizationID: e.OrganizationID})
			if err == nil && bl.SpentMinor+e.AmountMinor > bl.AllocatedMinor {
				if s.orgSettingBool(ctx, oc.OrgID, "org.finance.budget.hard_block", false) {
					return ErrBudgetOverrun
				}
				if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "BudgetLineExceeded",
					"budget_line", fromPg(e.BudgetLineID), oc.UserID, true,
					map[string]any{"budget_line_id": fromPg(e.BudgetLineID),
						"expense_id": eid, "over_by_minor": bl.SpentMinor + e.AmountMinor - bl.AllocatedMinor}); err != nil {
					return err
				}
			}
		}
		// spend = a real payment row (channel manual — disbursed out of the purse).
		p, err := q.CreatePayment(ctx, db.CreatePaymentParams{
			OrganizationID: e.OrganizationID, MembershipID: toPg(oc.MembershipID),
			AmountMinor: e.AmountMinor, Currency: e.Currency,
			Channel: channel, ExternalRef: pgText("expense:" + eid.String()[:8]),
			RecordedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		row, err := q.DisburseExpense(ctx, db.DisburseExpenseParams{
			ID: toPg(eid), OrganizationID: e.OrganizationID,
			PaymentID: p.ID, RequestedBy: toPg(oc.MembershipID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		// ledger debit + spent projection, atomically.
		le, err := q.PostLedgerEntry(ctx, db.PostLedgerEntryParams{
			OrganizationID: e.OrganizationID, PaymentID: p.ID,
			Direction: "debit", Account: "general", AmountMinor: e.AmountMinor,
			Currency: e.Currency, Memo: pgText(memo), PostedBy: toPg(oc.MembershipID),
			BudgetLineID: e.BudgetLineID, ExpenseID: e.ID})
		if err != nil {
			return err
		}
		if e.BudgetLineID.Valid {
			if _, err := q.BumpBudgetLineSpent(ctx, db.BumpBudgetLineSpentParams{
				ID: e.BudgetLineID, SpentMinor: e.AmountMinor}); err != nil {
				return err
			}
		}
		// mark the backing payment confirmed — disbursement IS the spend
		// (system confirm: separation lives on the expense, not the row).
		if _, err := q.ConfirmSystemPayment(ctx, db.ConfirmSystemPaymentParams{
			ID: p.ID, OrganizationID: e.OrganizationID, ApprovedBy: toPg(oc.MembershipID)}); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ExpenseDisbursed",
			"expense", eid, oc.UserID, false,
			map[string]any{"expense_id": eid, "payment_id": fromPg(p.ID), "amount_minor": e.AmountMinor}); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "LedgerEntryPosted",
			"ledger_entry", fromPg(le.ID), oc.UserID, true,
			map[string]any{"entry_id": fromPg(le.ID), "amount_minor": le.AmountMinor, "account": le.Account}); err != nil {
			return err
		}
		_ = row
		return nil
	})
}

// ---------- invoices ----------

type InvoiceIn struct {
	MembershipID uuid.UUID `json:"membership_id"`
	PayeeName    string    `json:"payee_name"`
	PayeeContact string    `json:"payee_contact"`
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
	DueOn        string    `json:"due_on"` // YYYY-MM-DD
	Draft        bool      `json:"draft"`
}

func (s *Service) CreateInvoice(ctx context.Context, oc *tenant.OrgCtx, in InvoiceIn) (uuid.UUID, string, error) {
	if in.AmountMinor <= 0 || in.Currency == "" {
		return uuid.Nil, "", ErrBadValue
	}
	if in.MembershipID == uuid.Nil && in.PayeeName == "" {
		return uuid.Nil, "", ErrBadValue // member or external payee required
	}
	if in.MembershipID != uuid.Nil {
		if org, err := s.q.GetMembershipOrg(ctx, toPg(in.MembershipID)); err != nil || fromPg(org) != oc.OrgID {
			return uuid.Nil, "", ErrNotFound
		}
	}
	var due pgtype.Date
	if in.DueOn != "" {
		if t, err := time.Parse("2006-01-02", in.DueOn); err == nil {
			due = pgtype.Date{Time: t, Valid: true}
		}
	}
	var row db.Invoices
	err := s.withTx(ctx, func(q *db.Queries) error {
		seq, err := q.NextInvoiceSeq(ctx, toPg(oc.OrgID))
		if err != nil {
			return err
		}
		state := "issued"
		if in.Draft {
			state = "draft"
		}
		row, err = q.CreateInvoice(ctx, db.CreateInvoiceParams{
			OrganizationID: toPg(oc.OrgID), MembershipID: toPg(in.MembershipID),
			PayeeName: pgText(in.PayeeName), PayeeContact: pgText(in.PayeeContact),
			Number:      fmt.Sprintf("INV-%d-%05d", time.Now().Year(), seq),
			AmountMinor: in.AmountMinor, Currency: in.Currency,
			State: state, DueOn: due, IssuedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		if state == "issued" {
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "InvoiceIssued",
				"invoice", fromPg(row.ID), oc.UserID, true,
				map[string]any{"invoice_id": fromPg(row.ID), "member_id": in.MembershipID,
					"amount_minor": in.AmountMinor, "currency": in.Currency})
		}
		return nil
	})
	return fromPg(row.ID), row.Number, err
}

func (s *Service) ListInvoices(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.Invoices, error) {
	return s.q.ListInvoices(ctx, db.ListInvoicesParams{OrganizationID: toPg(oc.OrgID), Column2: state})
}

func (s *Service) GetInvoice(ctx context.Context, oc *tenant.OrgCtx, iid uuid.UUID) (*db.Invoices, int64, error) {
	inv, err := s.q.GetInvoice(ctx, db.GetInvoiceParams{ID: toPg(iid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, 0, ErrNotFound
	}
	paid, err := s.q.InvoicePaidTotal(ctx, inv.ID)
	return &inv, paid, err
}

// RefreshInvoiceState recomputes issued|partially_paid|paid from confirmed
// payments — called on payment confirm (the invoice never goes 'paid'
// without reconciled ledger money, §25).
func (s *Service) RefreshInvoiceState(ctx context.Context, q *db.Queries, invoiceID pgtype.UUID) error {
	if !invoiceID.Valid {
		return nil
	}
	paid, err := q.InvoicePaidTotal(ctx, invoiceID)
	if err != nil {
		return err
	}
	var org pgtype.UUID
	if inv, err := q.GetInvoiceAny(ctx, invoiceID); err == nil {
		org = inv.OrganizationID
		st := "issued"
		if inv.State == "draft" || inv.State == "void" {
			return nil // terminal/draft states don't derive
		}
		if paid > 0 && paid < inv.AmountMinor {
			st = "partially_paid"
		} else if paid >= inv.AmountMinor {
			st = "paid"
		}
		_, err = q.SetInvoiceState(ctx, db.SetInvoiceStateParams{
			ID: invoiceID, OrganizationID: org, State: st})
		return err
	}
	return err
}

func (s *Service) VoidInvoice(ctx context.Context, oc *tenant.OrgCtx, iid uuid.UUID, reason string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.VoidInvoice(ctx, db.VoidInvoiceParams{
			ID: toPg(iid), OrganizationID: toPg(oc.OrgID),
			VoidedBy: toPg(oc.MembershipID), VoidReason: pgText(reason)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "InvoiceVoided",
			"invoice", fromPg(row.ID), oc.UserID, true,
			map[string]any{"invoice_id": fromPg(row.ID), "by": oc.MembershipID, "reason": reason})
	})
}
