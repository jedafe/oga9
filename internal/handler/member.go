// Ref: oga9-engineering/sections/11-members.md + conventions/api-routes-catalog.md
// Members module: directory, applications + decisions, ADR-014 transitions,
// renewals, cards, import/export, /me/memberships, public apply.
package handler

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tasks"
	"github.com/jedafe/oga9/internal/tenant"
)

type MemberHandler struct {
	svc   *tenant.Service
	queue *asynq.Client
}

func NewMemberHandler(svc *tenant.Service, queue *asynq.Client) *MemberHandler {
	return &MemberHandler{svc: svc, queue: queue}
}

func memberErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenant.ErrInvalidTransition):
		httpx.WriteErr(w, 409, "invalid_state_transition", "that lifecycle transition isn't allowed from the current state")
	case errors.Is(err, tenant.ErrSponsorRequired):
		httpx.WriteErr(w, 400, "sponsor_required", "this organization requires a sponsor")
	case errors.Is(err, tenant.ErrApplicationNotFound):
		httpx.WriteErr(w, 404, "application_not_found", "no application with that id in this org")
	case errors.Is(err, tenant.ErrAlreadyApplied):
		httpx.WriteErr(w, 409, "application_exists", "an open application already exists for this user")
	case errors.Is(err, tenant.ErrAlreadyMember):
		httpx.WriteErr(w, 409, "already_a_member", "this person already holds a membership in the organization")
	case errors.Is(err, tenant.ErrBadScopeUnit):
		httpx.WriteErr(w, 400, "invalid_scope_unit", "unit must belong to this organization")
	case errors.Is(err, tenant.ErrBadValue):
		httpx.WriteErr(w, 400, "invalid_value", "invalid field value")
	case errors.Is(err, tenant.ErrForbidden):
		httpx.WriteErr(w, 403, "forbidden", "action not permitted")
	case errors.Is(err, tenant.ErrMemberNotActive):
		httpx.WriteErr(w, 409, "member_not_active", "member is not active")
	case errors.Is(err, tenant.ErrImportNotFound):
		httpx.WriteErr(w, 404, "import_not_found", "no import with that id")
	case errors.Is(err, tenant.ErrDuplicateRenewal):
		httpx.WriteErr(w, 409, "renewal_exists", "this membership already has a renewal for that period")
	case errors.Is(err, tenant.ErrRegistrationPending):
		httpx.WriteErr(w, 409, "registration_unpaid", "registration dues must be settled before registration")
	case errors.Is(err, tenant.ErrMemberLimitExceeded):
		httpx.WriteErr(w, 402, "limit_exceeded", "plan member limit reached — upgrade to activate more members")
	case errors.Is(err, tenant.ErrNotMember):
		httpx.WriteErr(w, 404, "not_a_member", "membership not found in this organization")
	case errors.Is(err, tenant.ErrBadCategory):
		httpx.WriteErr(w, 400, "invalid_category", "unknown member category")
	case errors.Is(err, tenant.ErrInviteNoContact):
		httpx.WriteErr(w, 400, "validation_failed", "email or phone required")
	default:
		orgErr(w, err)
	}
}

// Members — GET /orgs/{id}/members?category=&state=&unit= (view_any).
func (h *MemberHandler) Members(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var unitID uuid.UUID
	if u := r.URL.Query().Get("unit"); u != "" {
		var err error
		if unitID, err = uuid.Parse(u); err != nil {
			httpx.WriteErr(w, 400, "validation_failed", "unit must be a uuid")
			return
		}
	}
	rows, err := h.svc.Members(r.Context(), oc, r.URL.Query().Get("category"), r.URL.Query().Get("state"), unitID)
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

// Member — GET /orgs/{id}/members/{mid}: view_any, or the member's own row.
func (h *MemberHandler) Member(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	mid, err := uuid.Parse(chi.URLParam(r, "member_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if mid != oc.MembershipID && !h.svc.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.members.view_any") {
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
		return
	}
	m, err := h.svc.MemberDetail(r.Context(), oc, mid)
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, m)
}

// PatchMember — PATCH /orgs/{id}/members/{mid}: manage edits category +
// home-unit scope; a member may self-edit their home unit only (§11:
// limited fields for view_own). Category stays admin-level — it prices
// privileges, a member can't reclassify themselves.
func (h *MemberHandler) PatchMember(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	mid, err := uuid.Parse(chi.URLParam(r, "member_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Category string    `json:"category"`
		UnitID   uuid.UUID `json:"unit_id"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || (req.Category == "" && req.UnitID == uuid.Nil) {
		httpx.WriteErr(w, 400, "validation_failed", "category or unit_id required")
		return
	}
	self := mid == oc.MembershipID
	if !self && !h.svc.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.members.manage") {
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
		return
	}
	if self && req.Category != "" {
		httpx.WriteErr(w, 403, "forbidden", "members cannot change their own category")
		return
	}
	if err := h.svc.PatchMember(r.Context(), oc, mid, req.Category, req.UnitID); err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": mid})
}

// Transition — POST /orgs/{id}/members/{mid}/transition {to: suspend|
// reinstate|expel|resign|deceased|life|register|activate} (manage).
func (h *MemberHandler) Transition(w http.ResponseWriter, r *http.Request) {
	mid, err := uuid.Parse(chi.URLParam(r, "member_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		To string `json:"to"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.To == "" {
		httpx.WriteErr(w, 400, "validation_failed", "to required")
		return
	}
	oc := middleware.OrgCtx(r.Context())
	// Self-service exits: resign (active|suspended→resigned — suspended
	// members use /orgs/{id}/resign since they carry no OrgCtx) and
	// withdraw (applied|screened→withdrawn, per ADR-014 any→withdrawn
	// while pre-active).
	selfExit := mid == oc.MembershipID && (req.To == "resign" || req.To == "withdraw")
	if !selfExit && !h.svc.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.members.manage") {
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
		return
	}
	if err := h.svc.TransitionMember(r.Context(), oc, mid, req.To); err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": mid, "action": req.To})
}

// Resign — POST /orgs/{id}/resign: self-service exit for members whose
// state blocks OrgCtx (suspended members may still resign, §11). Sits
// outside the OrgCtx group on purpose — the service resolves the
// caller's own membership server-side.
func (h *MemberHandler) Resign(w http.ResponseWriter, r *http.Request) {
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "org_id must be a uuid")
		return
	}
	if err := h.svc.ResignSelf(r.Context(), middleware.UserID(r.Context()), orgID); err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"status": "resigned"})
}

// Renew — POST /orgs/{id}/members/{mid}/renew: manage, or the member
// themselves when a confirmed payment_id settles it (§11: manage or
// member+payment). Self-renew without payment stays blocked — it would
// mint free good-standing.
func (h *MemberHandler) Renew(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	mid, err := uuid.Parse(chi.URLParam(r, "member_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Period    string    `json:"period_label"`
		StartsAt  string    `json:"starts_at"`
		EndsAt    string    `json:"ends_at"`
		PaymentID uuid.UUID `json:"payment_id"` // §12 link — required for self-renew
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Period == "" {
		httpx.WriteErr(w, 400, "validation_failed", "period_label required")
		return
	}
	if !h.svc.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.members.manage") {
		if mid != oc.MembershipID || req.PaymentID == uuid.Nil {
			httpx.WriteErr(w, 403, "forbidden", "self-renewal requires a confirmed payment")
			return
		}
	}
	starts, err := time.Parse("2006-01-02", req.StartsAt)
	ends, err2 := time.Parse("2006-01-02", req.EndsAt)
	if err != nil || err2 != nil {
		httpx.WriteErr(w, 400, "validation_failed", "starts_at/ends_at must be YYYY-MM-DD")
		return
	}
	id, err := h.svc.RenewMember(r.Context(), oc, mid, req.Period, starts, ends, req.PaymentID)
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "period_label": req.Period})
}

// Card — POST /orgs/{id}/members/{mid}/card (card.issue): issue a member card.
func (h *MemberHandler) Card(w http.ResponseWriter, r *http.Request) {
	mid, err := uuid.Parse(chi.URLParam(r, "member_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Design json.RawMessage `json:"design"`
	}
	httpx.Decode(w, r, &req) // design optional
	card, err := h.svc.IssueCard(r.Context(), middleware.OrgCtx(r.Context()), mid, req.Design)
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 201, card)
}

// History — GET /orgs/{id}/members/{mid}/history (view_any): sourced event stream.
func (h *MemberHandler) History(w http.ResponseWriter, r *http.Request) {
	mid, err := uuid.Parse(chi.URLParam(r, "member_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	rows, err := h.svc.MemberHistory(r.Context(), middleware.OrgCtx(r.Context()), mid)
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

// Apply — POST /orgs/{id}/applications (manage): admin-assisted application.
func (h *MemberHandler) Apply(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var req struct {
		UserID   uuid.UUID       `json:"user_id"`
		Email    string          `json:"email"`
		Phone    string          `json:"phone"`
		Answers  json.RawMessage `json:"answers"`
		Sponsors []uuid.UUID     `json:"sponsor_member_ids"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.Apply(r.Context(), oc.OrgID, req.UserID, req.Email, req.Phone, req.Answers, req.Sponsors)
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "status": "applied"})
}

// Applications — GET /orgs/{id}/applications?status= (approve node): queue.
func (h *MemberHandler) Applications(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.Applications(r.Context(), middleware.OrgCtx(r.Context()), r.URL.Query().Get("status"))
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

// Decide — POST /orgs/{id}/applications/{aid}/decision {action: screen|
// approve|reject|withdraw} (approve node).
func (h *MemberHandler) Decide(w http.ResponseWriter, r *http.Request) {
	aid, err := uuid.Parse(chi.URLParam(r, "application_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Action   string      `json:"action"`
		Sponsors []uuid.UUID `json:"sponsor_member_ids"` // screener-attached at decision time
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Action == "" {
		httpx.WriteErr(w, 400, "validation_failed", "action required")
		return
	}
	if err := h.svc.DecideApplication(r.Context(), middleware.OrgCtx(r.Context()), aid, req.Action, req.Sponsors); err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": aid, "action": req.Action})
}

// Withdraw — POST /orgs/{id}/applications/{aid}/withdraw (self): the
// applicant's own row only. Outside the orgCtx group on purpose — a
// pre-decision applicant has no membership yet.
func (h *MemberHandler) Withdraw(w http.ResponseWriter, r *http.Request) {
	orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	appID, err := uuid.Parse(chi.URLParam(r, "application_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.WithdrawApplication(r.Context(), middleware.UserID(r.Context()), orgID, appID); err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": appID, "status": "withdrawn"})
}

// PublicApply — POST /directory/orgs/{slug}/apply: unauthenticated intake.
func (h *MemberHandler) PublicApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Email   string          `json:"email"`
		Phone   string          `json:"phone"`
		Answers json.RawMessage `json:"answers"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.PublicApply(r.Context(), chi.URLParam(r, "slug"), req.Email, req.Phone, req.Answers)
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "status": "applied"})
}

// Import — POST /orgs/{id}/members/import {filename, csv | rows}:
// durable import row + membership:import fan-out (§11: rows become
// applications). csv is raw text with a header row (email,phone,…);
// rows is a pre-parsed JSON array for API clients.
func (h *MemberHandler) Import(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var req struct {
		Filename string            `json:"filename"`
		CSV      string            `json:"csv"`
		Rows     []json.RawMessage `json:"rows"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Filename == "" || (req.CSV == "" && len(req.Rows) == 0) {
		httpx.WriteErr(w, 400, "validation_failed", "filename and csv or rows required")
		return
	}
	var raw json.RawMessage
	var err error
	if req.CSV != "" {
		raw, err = csvRows(req.CSV)
	} else {
		raw, err = json.Marshal(req.Rows)
	}
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "unparseable import payload: "+err.Error())
		return
	}
	id, err := h.svc.CreateImport(r.Context(), oc, req.Filename, raw)
	if err != nil {
		memberErr(w, err)
		return
	}
	task, err := tasks.NewMemberImportTask(id)
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "task build failed")
		return
	}
	if _, err := h.queue.EnqueueContext(r.Context(), task); err != nil {
		// The durable row exists — fail it visibly rather than stranding it
		// at 'processing' until the stale-import sweep finds it.
		_ = h.svc.FailImport(r.Context(), id, err.Error())
		httpx.WriteErr(w, 500, "internal_error", "import enqueue failed")
		return
	}
	httpx.WriteData(w, 202, map[string]any{"id": id, "status": "processing"})
}

// Export — GET /orgs/{id}/members/export (export node): synchronous CSV —
// the async membership:export file-delivery path lands with blob storage.
func (h *MemberHandler) Export(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.Members(r.Context(), middleware.OrgCtx(r.Context()), "", "", uuid.Nil)
	if err != nil {
		memberErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", `attachment; filename="members.csv"`)
	cw := csv.NewWriter(w)
	cw.Write([]string{"member_no", "display_name", "email", "category", "lifecycle_state", "dues_standing", "joined_at"})
	for _, m := range rows {
		joined := ""
		if m.JoinedAt.Valid {
			joined = m.JoinedAt.Time.Format("2006-01-02")
		}
		cw.Write([]string{m.MemberNo.String, m.DisplayName, m.Email, m.Category, m.LifecycleState, m.DuesStanding, joined})
	}
	cw.Flush()
}

// MyMemberships — GET /me/memberships (auth): all orgs for the caller.
// Each row carries the computed eligibility flag (§11: active/life_member
// + good dues standing) — never a stored column.
func (h *MemberHandler) MyMemberships(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.MyMemberships(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		memberErr(w, err)
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, m := range rows {
		out = append(out, map[string]any{
			"id": m.ID, "organization_id": m.OrganizationID, "org_name": m.OrgName,
			"org_slug": m.OrgSlug, "member_no": m.MemberNo, "category": m.Category,
			"lifecycle_state": m.LifecycleState, "dues_standing": m.DuesStanding,
			"joined_at": m.JoinedAt,
			"eligible":  tenant.Eligible(m.LifecycleState, m.DuesStanding),
		})
	}
	httpx.WriteData(w, 200, out)
}

// ApplyForm — GET /directory/orgs/{slug}/apply (public): the org's
// configurable application fields + sponsor requirement so the form can
// render before submission.
func (h *MemberHandler) ApplyForm(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.PublicApplyForm(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		memberErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

// csvRows parses a header-row CSV into the row-object shape the worker
// stores in member_imports.data.
func csvRows(text string) (json.RawMessage, error) {
	r := csv.NewReader(strings.NewReader(text))
	recs, err := r.ReadAll()
	if err != nil || len(recs) < 2 {
		return nil, fmt.Errorf("need a header row and at least one data row")
	}
	head := recs[0]
	for i := range head {
		head[i] = strings.ToLower(strings.TrimSpace(head[i]))
	}
	rows := make([]map[string]any, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		row := map[string]any{}
		for i, cell := range rec {
			if i < len(head) && head[i] != "" {
				row[head[i]] = strings.TrimSpace(cell)
			}
		}
		rows = append(rows, row)
	}
	return json.Marshal(rows)
}

// ExportAsync — POST /orgs/{id}/members/export {filename}: durable row +
// membership:export fan-out; GET /members/exports/{eid} downloads it.
func (h *MemberHandler) ExportAsync(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var req struct {
		Filename string `json:"filename"`
	}
	_ = httpx.Decode(w, r, &req)
	id, err := h.svc.RequestExport(r.Context(), oc, req.Filename)
	if err != nil {
		memberErr(w, err)
		return
	}
	task, err := tasks.NewMemberExportTask(id)
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "task build failed")
		return
	}
	if _, err := h.queue.EnqueueContext(r.Context(), task); err != nil {
		httpx.WriteErr(w, 500, "internal_error", "export enqueue failed")
		return
	}
	httpx.WriteData(w, 202, map[string]any{"id": id, "status": "processing"})
}

// ExportDownload — GET /orgs/{id}/members/exports/{eid}: the stored CSV.
func (h *MemberHandler) ExportDownload(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	eid, err := uuid.Parse(chi.URLParam(r, "export_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	e, err := h.svc.ExportDownload(r.Context(), oc, eid)
	if err != nil {
		memberErr(w, err)
		return
	}
	if e.Status != "done" {
		httpx.WriteData(w, 200, map[string]any{"id": eid, "status": e.Status})
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	fname := sanitizeHeaderValue(e.Filename)
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.Write(e.Content)
}

// sanitizeHeaderValue strips quote/CR/LF — a filename or title must
// never inject a second header through Content-Disposition.
func sanitizeHeaderValue(v string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '"', '\\', '\r', '\n':
			return -1
		}
		return r
	}, v)
}
