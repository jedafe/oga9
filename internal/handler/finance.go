// Ref: oga9-engineering/sections/12-finance-lite.md — finance-lite routes.
// Rule 9: POST /payments requires Idempotency-Key. Approval paths rely on
// the service's recorder!=approver CAS (separation of duties).
package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jedafe/oga9/internal/finance"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tenant"
)

type FinanceHandler struct {
	svc *finance.Service
	org *tenant.Service
}

func NewFinanceHandler(svc *finance.Service, org *tenant.Service) *FinanceHandler {
	return &FinanceHandler{svc: svc, org: org}
}

func finErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, finance.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "no such finance record")
	case errors.Is(err, finance.ErrInvalidTransition):
		httpx.WriteErr(w, 409, "invalid_state_transition", "action not allowed in current state")
	case errors.Is(err, finance.ErrSeparation):
		httpx.WriteErr(w, 409, "separation_of_duties", "recorder and approver must differ")
	case errors.Is(err, finance.ErrAlreadyClaimed):
		httpx.WriteErr(w, 409, "claim_duplicate", "a claim with that reference already exists")
	case errors.Is(err, finance.ErrDriveKind):
		httpx.WriteErr(w, 400, "fixed_amount_required", "fixed_amount drives need an amount")
	case errors.Is(err, finance.ErrIdempotencyConflict):
		httpx.WriteErr(w, 409, "idempotency_key_in_use", "that Idempotency-Key is bound to another payment")
	case errors.Is(err, finance.ErrBadChannel):
		httpx.WriteErr(w, 400, "invalid_channel", "channel must be cash|transfer|pos|other_manual")
	case errors.Is(err, finance.ErrBadValue):
		httpx.WriteErr(w, 400, "validation_failed", err.Error())
	default:
		log.Printf("finance: %v", err) // detail stays server-side
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

// pathUUID parses a chi path param — a malformed id is a 400, not a
// silent Nil that surfaces later as a confusing 404/409.
func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, name))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", name+" must be a uuid")
		return uuid.Nil, false
	}
	return id, true
}

// statementGate enforces org.finance.statement_visibility: 'members'
// (default) needs statement_view only — already middleware-gated; 'exco'
// upgrades the bar to reports.read.
func (h *FinanceHandler) statementGate(w http.ResponseWriter, r *http.Request, oc *tenant.OrgCtx) bool {
	if h.svc.StatementVisibility(r.Context(), oc.OrgID) == "exco" &&
		!h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.finance.reports.read") {
		httpx.WriteErr(w, 403, "forbidden", "statement visibility is restricted to officers")
		return false
	}
	return true
}

// ---------- contribution types / schedules ----------

func (h *FinanceHandler) ContributionTypes(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListContributionTypes(r.Context(), oc)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) CreateContributionType(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var req struct {
		Key, Label, Kind string
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Key == "" || req.Label == "" {
		httpx.WriteErr(w, 400, "validation_failed", "key and label required")
		return
	}
	id, err := h.svc.CreateContributionType(r.Context(), oc, req.Key, req.Label, req.Kind)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *FinanceHandler) Schedules(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListSchedules(r.Context(), oc)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) CreateSchedule(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in finance.ScheduleIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.CreateSchedule(r.Context(), oc, in)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

// UpdateSchedule — PATCH /orgs/{id}/finance/schedules/{sid}: full edit
// surface (catalog PATCH) — supplied fields update, omitted keep values;
// explicit {"active":false} deactivates, {"ends_on":null} clears the end.
func (h *FinanceHandler) UpdateSchedule(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "schedule_id")
	if !ok {
		return
	}
	var req struct {
		Name        *string         `json:"name"`
		AmountMinor *int64          `json:"amount_minor"`
		Currency    string          `json:"currency"`
		Recurrence  *string         `json:"recurrence"`
		AppliesTo   json.RawMessage `json:"applies_to"`
		StartsOn    *time.Time      `json:"starts_on"`
		EndsOn      json.RawMessage `json:"ends_on"` // null clears; absent keeps
		Active      *bool           `json:"active"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	in := finance.ScheduleIn{AppliesTo: req.AppliesTo, Currency: req.Currency}
	if req.Name != nil {
		in.Name = *req.Name
	}
	if req.AmountMinor != nil {
		in.AmountMinor = *req.AmountMinor
	}
	if req.Recurrence != nil {
		in.Recurrence = *req.Recurrence
	}
	if req.StartsOn != nil {
		in.StartsOn = *req.StartsOn
	}
	clearEnds := false
	if req.EndsOn != nil {
		if string(req.EndsOn) == "null" {
			clearEnds = true
		} else if err := json.Unmarshal(req.EndsOn, &in.EndsOn); err != nil {
			httpx.WriteErr(w, 400, "validation_failed", "ends_on must be a timestamp or null")
			return
		}
	}
	if err := h.svc.UpdateSchedule(r.Context(), oc, sid, in, req.Active, clearEnds); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": sid})
}

// ---------- obligations ----------

func (h *FinanceHandler) Obligations(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	mid, _ := uuid.Parse(r.URL.Query().Get("member"))
	list, err := h.svc.ListObligations(r.Context(), oc, mid,
		r.URL.Query().Get("status"), r.URL.Query().Get("period"))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) WaiveObligation(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	oid, ok := pathUUID(w, r, "obligation_id")
	if !ok {
		return
	}
	if err := h.svc.WaiveObligation(r.Context(), oc, oid); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": oid, "status": "waived"})
}

// ---------- payments ----------

func (h *FinanceHandler) RecordPayment(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in finance.PaymentIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	in.IdempotencyKey = r.Header.Get("Idempotency-Key")
	if in.IdempotencyKey == "" {
		httpx.WriteErr(w, 400, "idempotency_key_required", "Idempotency-Key header required")
		return
	}
	self := in.MembershipID == uuid.Nil || in.MembershipID == oc.MembershipID
	if !self && !h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.finance.payments.record") {
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
		return
	}
	id, replay, err := h.svc.RecordPayment(r.Context(), oc, in)
	if err != nil {
		finErr(w, err)
		return
	}
	status := 201
	if replay {
		status = 200 // idempotent replay — original stands
	}
	httpx.WriteData(w, status, map[string]any{"id": id, "status": "initiated"})
}

func (h *FinanceHandler) ConfirmPayment(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "payment_id")
	if !ok {
		return
	}
	if err := h.svc.ConfirmPayment(r.Context(), oc, pid); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": pid, "status": "confirmed"})
}

func (h *FinanceHandler) ReversePayment(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "payment_id")
	if !ok {
		return
	}
	var req struct {
		Memo string `json:"memo"`
	}
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.ReversePayment(r.Context(), oc, pid, req.Memo); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": pid, "status": "reversed"})
}

func (h *FinanceHandler) ReconcilePayment(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "payment_id")
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.ReconcilePayment(r.Context(), oc, pid, req.Note); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": pid, "status": "reconciled"})
}

func (h *FinanceHandler) Receipt(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	rid, ok := pathUUID(w, r, "receipt_id")
	if !ok {
		return
	}
	rc, err := h.svc.GetReceipt(r.Context(), oc, rid)
	if err != nil {
		finErr(w, err)
		return
	}
	// view_own / reports.read: the payee reads their own receipt; the
	// reports node reads anyone's (§12 gate list).
	if fromPgUUID(rc.IssuedTo) != oc.MembershipID &&
		!h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.finance.reports.read") {
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
		return
	}
	httpx.WriteData(w, 200, rc)
}

// ---------- claims ----------

func (h *FinanceHandler) SubmitClaim(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in finance.PaymentIn
	var req struct {
		finance.PaymentIn
		Note           string    `json:"note"`
		ReceiptMediaID uuid.UUID `json:"receipt_media_id"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	in = req.PaymentIn
	id, err := h.svc.SubmitClaim(r.Context(), oc, in, req.Note, req.ReceiptMediaID)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "state": "submitted"})
}

func (h *FinanceHandler) Claims(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListClaims(r.Context(), oc, r.URL.Query().Get("state"))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// MyClaims — GET /me/claims: the user's claims across all memberships
// (authed, not org-scoped — the catalog route is /me/claims).
func (h *FinanceHandler) MyClaims(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.MyClaims(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) ConfirmClaim(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, ok := pathUUID(w, r, "claim_id")
	if !ok {
		return
	}
	var req struct {
		Note         string    `json:"note"`
		DriveID      uuid.UUID `json:"drive_id"`      // officer allocation on confirm (ad-hoc claims)
		ObligationID uuid.UUID `json:"obligation_id"` // officer allocation on confirm
	}
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.ConfirmClaim(r.Context(), oc, cid, req.Note, req.DriveID, req.ObligationID); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": cid, "state": "confirmed"})
}

func (h *FinanceHandler) RejectClaim(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, ok := pathUUID(w, r, "claim_id")
	if !ok {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.RejectClaim(r.Context(), oc, cid, req.Note); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": cid, "state": "rejected"})
}

// ---------- drives ----------

func (h *FinanceHandler) Drives(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListDrives(r.Context(), oc, r.URL.Query().Get("state"))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) OpenDrive(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in finance.DriveIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.OpenDrive(r.Context(), oc, in)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "state": "open"})
}

func fromPgUUID(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }

// PatchDrive — PATCH /orgs/{id}/finance/drives/{did}: open drives only
// (title/purpose/closes_at), drives.manage.
func (h *FinanceHandler) PatchDrive(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, ok := pathUUID(w, r, "drive_id")
	if !ok {
		return
	}
	var req struct {
		Title    *string    `json:"title"`
		Purpose  *string    `json:"purpose"`
		ClosesAt *time.Time `json:"closes_at"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if req.Title == nil && req.Purpose == nil && req.ClosesAt == nil {
		httpx.WriteErr(w, 400, "validation_failed", "nothing to update")
		return
	}
	if err := h.svc.PatchDrive(r.Context(), oc, did, req.Title, req.Purpose, req.ClosesAt); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": did})
}

func (h *FinanceHandler) Drive(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, ok := pathUUID(w, r, "drive_id")
	if !ok {
		return
	}
	d, total, contributors, err := h.svc.DriveDetail(r.Context(), oc, did)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{
		"drive": d, "collected_minor": total, "contributors": contributors})
}

func (h *FinanceHandler) CloseDrive(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, ok := pathUUID(w, r, "drive_id")
	if !ok {
		return
	}
	if err := h.svc.CloseDrive(r.Context(), oc, did); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": did, "state": "closed"})
}

func (h *FinanceHandler) DisburseDrive(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, ok := pathUUID(w, r, "drive_id")
	if !ok {
		return
	}
	var req struct {
		Memo string `json:"memo"`
	}
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.DisburseDrive(r.Context(), oc, did, req.Memo); err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": did, "state": "disbursed"})
}

func (h *FinanceHandler) DriveContributions(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, ok := pathUUID(w, r, "drive_id")
	if !ok {
		return
	}
	// Reviewers (claims.review | drives.manage) always see full provenance;
	// members see what the org + claimant allow (§12 drive transparency).
	canReview := h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.finance.claims.review") ||
		h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.finance.drives.manage")
	list, err := h.svc.DriveContributions(r.Context(), oc, did, canReview)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// ---------- reads ----------

func (h *FinanceHandler) Summary(w http.ResponseWriter, r *http.Request) {
	t, err := h.svc.Summary(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, t)
}

func (h *FinanceHandler) Defaulters(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Defaulters(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) Ledger(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	if !h.statementGate(w, r, oc) {
		return
	}
	list, err := h.svc.Ledger(r.Context(), oc, r.URL.Query().Get("account"))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *FinanceHandler) Statement(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	if !h.statementGate(w, r, oc) {
		return
	}
	list, err := h.svc.Statement(r.Context(), oc)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// ObligationsStatus — GET /orgs/{id}/finance/obligations/status: the
// who-paid roll call (?schedule=&period=&type= filters).
func (h *FinanceHandler) ObligationsStatus(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	if !h.statementGate(w, r, oc) {
		return
	}
	sid, _ := uuid.Parse(r.URL.Query().Get("schedule"))
	tid, _ := uuid.Parse(r.URL.Query().Get("type"))
	list, err := h.svc.ObligationsStatus(r.Context(), oc, sid,
		r.URL.Query().Get("period"), tid)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// ScheduleStatus — GET /orgs/{id}/finance/schedules/{sid}/status: the
// per-schedule roll call.
func (h *FinanceHandler) ScheduleStatus(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	if !h.statementGate(w, r, oc) {
		return
	}
	sid, ok := pathUUID(w, r, "schedule_id")
	if !ok {
		return
	}
	list, err := h.svc.ObligationsStatus(r.Context(), oc, sid, "", uuid.Nil)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// Contributions — GET /orgs/{id}/finance/contributions: per-type history.
func (h *FinanceHandler) Contributions(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	if !h.statementGate(w, r, oc) {
		return
	}
	list, err := h.svc.Contributions(r.Context(), oc)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// Donations — GET /orgs/{id}/activities/{aid}/donations: event-linked
// contribution history.
func (h *FinanceHandler) Donations(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	if !h.statementGate(w, r, oc) {
		return
	}
	aid, ok := pathUUID(w, r, "activity_id")
	if !ok {
		return
	}
	list, err := h.svc.Donations(r.Context(), oc, aid)
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// MyFinance — GET /me/finance: obligations + payments across every
// membership the user holds (authed, not org-scoped).
func (h *FinanceHandler) MyFinance(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.MyFinance(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		finErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}
