// Ref: oga9-engineering/sections/25-finance-full.md — budget, expense,
// invoice routes. Separation of duties is enforced in the service.
package handler

import (
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/finance"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

// ---------- budgets ----------

func (h *FinanceHandler) Budgets(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListBudgets(r.Context(), oc)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) CreateBudget(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in finance.BudgetIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.CreateBudget(r.Context(), oc, in)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *FinanceHandler) GetBudget(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	bid, ok := pathUUID(w, r, "budget_id")
	if !ok {
		return
	}
	b, lines, err := h.svc.GetBudget(r.Context(), oc, bid)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"budget": b, "lines": lines})
}

func (h *FinanceHandler) PatchBudget(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	bid, ok := pathUUID(w, r, "budget_id")
	if !ok {
		return
	}
	var in struct {
		Name       *string `json:"name"`
		FiscalYear *int32  `json:"fiscal_year"`
		Lines      []struct {
			ID             uuid.UUID `json:"id"`
			Label          string    `json:"label"`
			AllocatedMinor int64     `json:"allocated_minor"`
			Currency       string    `json:"currency"`
		} `json:"lines"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if err := h.svc.PatchBudget(r.Context(), oc, bid, in.Name, in.FiscalYear, in.Lines); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": bid})
}

func (h *FinanceHandler) AdoptBudget(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	bid, ok := pathUUID(w, r, "budget_id")
	if !ok {
		return
	}
	if err := h.svc.AdoptBudget(r.Context(), oc, bid); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "adopted"})
}

func (h *FinanceHandler) CloseBudget(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	bid, ok := pathUUID(w, r, "budget_id")
	if !ok {
		return
	}
	if err := h.svc.CloseBudget(r.Context(), oc, bid); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "closed"})
}

func (h *FinanceHandler) VsActual(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	bid, ok := pathUUID(w, r, "budget_id")
	if !ok {
		return
	}
	rows, err := h.svc.VsActual(r.Context(), oc, bid)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

// ---------- expenses ----------

func (h *FinanceHandler) Expenses(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListExpenses(r.Context(), oc, r.URL.Query().Get("state"))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) RequestExpense(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in finance.ExpenseIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.RequestExpense(r.Context(), oc, in)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *FinanceHandler) GetExpense(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	eid, ok := pathUUID(w, r, "expense_id")
	if !ok {
		return
	}
	e, appr, err := h.svc.GetExpense(r.Context(), oc, eid)
	if err != nil {
		finErr(w, err)
		return
	}
	// §25: reports.read sees all; the requester sees their own.
	if uuid.UUID(e.RequestedBy.Bytes) != oc.MembershipID &&
		!h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.finance.reports.read") {
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
		return
	}
	httpx.WriteData(w, 200, map[string]any{"expense": e, "approvals": appr})
}

func (h *FinanceHandler) decideExpense(w http.ResponseWriter, r *http.Request, approve bool) {
	oc := middleware.OrgCtx(r.Context())
	eid, ok := pathUUID(w, r, "expense_id")
	if !ok {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	_ = httpx.Decode(w, r, &in)
	var err error
	if approve {
		err = h.svc.ApproveExpense(r.Context(), oc, eid, in.Note)
	} else {
		err = h.svc.RejectExpense(r.Context(), oc, eid, in.Note)
	}
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": eid})
}

func (h *FinanceHandler) ApproveExpense(w http.ResponseWriter, r *http.Request) {
	h.decideExpense(w, r, true)
}
func (h *FinanceHandler) RejectExpense(w http.ResponseWriter, r *http.Request) {
	h.decideExpense(w, r, false)
}

func (h *FinanceHandler) DisburseExpense(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	eid, ok := pathUUID(w, r, "expense_id")
	if !ok {
		return
	}
	var in struct {
		Channel string `json:"channel"`
		Memo    string `json:"memo"`
	}
	_ = httpx.Decode(w, r, &in)
	if in.Channel == "" {
		in.Channel = "transfer"
	}
	if err := h.svc.DisburseExpense(r.Context(), oc, eid, in.Channel, in.Memo); err != nil {
		if errors.Is(err, finance.ErrBudgetOverrun) {
			httpx.WriteErr(w, 409, "budget_hard_block", "expense exceeds the budget line allocation")
			return
		}
		if errors.Is(err, finance.ErrSeparationExp) {
			httpx.WriteErr(w, 409, "separation_of_duties", "requester/approver/disburser must differ")
			return
		}
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "disbursed"})
}

// ---------- invoices ----------

func (h *FinanceHandler) Invoices(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListInvoices(r.Context(), oc, r.URL.Query().Get("state"))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) CreateInvoice(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in finance.InvoiceIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, num, err := h.svc.CreateInvoice(r.Context(), oc, in)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "number": num})
}

func (h *FinanceHandler) GetInvoice(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	iid, ok := pathUUID(w, r, "invoice_id")
	if !ok {
		return
	}
	inv, paid, err := h.svc.GetInvoice(r.Context(), oc, iid)
	if err != nil {
		finErr(w, err)
		return
	}
	// §25: reports.read sees all; the billed member sees their own.
	if uuid.UUID(inv.MembershipID.Bytes) != oc.MembershipID &&
		!h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.finance.reports.read") {
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
		return
	}
	httpx.WriteData(w, 200, map[string]any{"invoice": inv, "paid_minor": paid})
}

func (h *FinanceHandler) VoidInvoice(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	iid, ok := pathUUID(w, r, "invoice_id")
	if !ok {
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = httpx.Decode(w, r, &in)
	if err := h.svc.VoidInvoice(r.Context(), oc, iid, in.Reason); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "void"})
}
