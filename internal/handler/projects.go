// Ref: oga9-engineering/sections/20-projects.md — §20 project routes.
package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/projects"
)

type ProjectsHandler struct {
	svc *projects.Service
}

func NewProjectsHandler(s *projects.Service) *ProjectsHandler {
	return &ProjectsHandler{svc: s}
}

func projectErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, projects.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, projects.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, projects.ErrBadState):
		httpx.WriteErr(w, 409, "bad_project_state", "project is not in a state for that action")
	case errors.Is(err, projects.ErrNoResolution):
		httpx.WriteErr(w, 409, "resolution_required", "a passed resolution is required for this transition")
	default:
		log.Printf("projects: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")

	case errors.Is(err, projects.ErrOverBudget):
		httpx.WriteErr(w, 409, "over_project_budget", "spend exceeds the project budget")
	case errors.Is(err, projects.ErrOverMandate):
		httpx.WriteErr(w, 409, "over_mandate_limit", "spend exceeds the committee mandate")
	case errors.Is(err, projects.ErrBadPayment):
		httpx.WriteErr(w, 422, "payment_not_found", "payment must be a real confirmed payment")
	}
}

func projectID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "project_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return uuid.Nil, false
	}
	return id, true
}

type projectReq struct {
	Title       string    `json:"title"`
	Description string    `json:"description"`
	UnitID      uuid.UUID `json:"unit_id"`
	StartsOn    time.Time `json:"starts_on"`
	TargetOn    time.Time `json:"target_on"`
}

func (h *ProjectsHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.List(r.Context(), middleware.OrgCtx(r.Context()), r.URL.Query().Get("state"))
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *ProjectsHandler) Propose(w http.ResponseWriter, r *http.Request) {
	var req projectReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Title == "" {
		httpx.WriteErr(w, 400, "validation_failed", "title required")
		return
	}
	id, err := h.svc.Propose(r.Context(), middleware.OrgCtx(r.Context()),
		req.Title, req.Description, req.UnitID, req.StartsOn, req.TargetOn)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"project_id": id})
}

func (h *ProjectsHandler) Get(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Get(r.Context(), middleware.OrgCtx(r.Context()), pid)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

type transitionReq struct {
	Action       string    `json:"action"` // start|resume|suspend|abandon
	ResolutionID uuid.UUID `json:"resolution_id"`
}

func (h *ProjectsHandler) Transition(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	var req transitionReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Action == "" {
		httpx.WriteErr(w, 400, "validation_failed", "action required")
		return
	}
	var err error
	if req.Action == "approve" {
		err = h.svc.Approve(r.Context(), middleware.OrgCtx(r.Context()), pid, req.ResolutionID)
	} else {
		err = h.svc.Transition(r.Context(), middleware.OrgCtx(r.Context()), pid, req.Action)
	}
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"done": true})
}

type approveReq struct {
	ResolutionID uuid.UUID `json:"resolution_id"`
}

// Approve — POST /projects/{pid}/approve: dedicated resolution-gate
// route; takes resolution_id directly (no redundant action field).
func (h *ProjectsHandler) Approve(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	var req approveReq
	if err := httpx.Decode(w, r, &req); err != nil || req.ResolutionID == uuid.Nil {
		httpx.WriteErr(w, 400, "validation_failed", "resolution_id required")
		return
	}
	if err := h.svc.Approve(r.Context(), middleware.OrgCtx(r.Context()), pid, req.ResolutionID); err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"approved": true})
}

type milestoneReq struct {
	Title  string    `json:"title"`
	Weight int32     `json:"weight"`
	DueOn  time.Time `json:"due_on"`
}

func (h *ProjectsHandler) ListMilestones(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.ListMilestones(r.Context(), middleware.OrgCtx(r.Context()), pid)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *ProjectsHandler) AddMilestone(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	var req milestoneReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Title == "" {
		httpx.WriteErr(w, 400, "validation_failed", "title required")
		return
	}
	if req.Weight == 0 {
		req.Weight = 1
	}
	id, err := h.svc.AddMilestone(r.Context(), middleware.OrgCtx(r.Context()), pid,
		req.Title, req.Weight, req.DueOn)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"milestone_id": id})
}

func (h *ProjectsHandler) CompleteMilestone(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	mid, err := uuid.Parse(chi.URLParam(r, "milestone_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.CompleteMilestone(r.Context(), middleware.OrgCtx(r.Context()), pid, mid); err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"completed": true})
}

type updateReq struct {
	Body        string `json:"body"`
	ProgressPct int32  `json:"progress_pct"`
}

func (h *ProjectsHandler) PostUpdate(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	var req updateReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Body == "" {
		httpx.WriteErr(w, 400, "validation_failed", "body required")
		return
	}
	id, err := h.svc.PostUpdate(r.Context(), middleware.OrgCtx(r.Context()), pid, req.Body, req.ProgressPct)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"update_id": id})
}

func (h *ProjectsHandler) ListUpdates(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.ListUpdates(r.Context(), middleware.OrgCtx(r.Context()), pid)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

type vendorReq struct {
	Name    string          `json:"name"`
	Kind    string          `json:"kind"`
	Contact json.RawMessage `json:"contact"`
}

func (h *ProjectsHandler) ListVendors(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListVendors(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *ProjectsHandler) AddVendor(w http.ResponseWriter, r *http.Request) {
	var req vendorReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Name == "" {
		httpx.WriteErr(w, 400, "validation_failed", "name required")
		return
	}
	id, err := h.svc.AddVendor(r.Context(), middleware.OrgCtx(r.Context()), req.Name, req.Kind, req.Contact)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"vendor_id": id})
}

type donorReq struct {
	DonorName    string    `json:"donor_name"`
	MembershipID uuid.UUID `json:"membership_id"`
	AmountMinor  int64     `json:"amount_minor"`
	Currency     string    `json:"currency"`
	PaymentID    uuid.UUID `json:"payment_id"`
}

func (h *ProjectsHandler) RecordDonor(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	var req donorReq
	if err := httpx.Decode(w, r, &req); err != nil || req.DonorName == "" {
		httpx.WriteErr(w, 400, "validation_failed", "donor_name required")
		return
	}
	id, err := h.svc.RecordDonor(r.Context(), middleware.OrgCtx(r.Context()), pid,
		req.DonorName, req.MembershipID, req.AmountMinor, req.Currency, req.PaymentID)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"donor_id": id})
}

func (h *ProjectsHandler) Graph(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.Graph(r.Context(), middleware.OrgCtx(r.Context()), pid)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *ProjectsHandler) Close(w http.ResponseWriter, r *http.Request) {
	pid, ok := projectID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Close(r.Context(), middleware.OrgCtx(r.Context()), pid); err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "completed"})
}

// PublicProjects — GET /directory/orgs/{slug}/projects (no auth).
func (h *ProjectsHandler) PublicProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.PublicProjects(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

// RecordSpend — POST /projects/{pid}/spend: posts a project expense to
// finance (never off-ledger); committee-owned projects are capped by the
// committee_mandates budget_limit (§16).
func (h *ProjectsHandler) RecordSpend(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "project_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Memo        string `json:"memo"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.AmountMinor <= 0 || req.Currency == "" {
		httpx.WriteErr(w, 400, "validation_failed", "amount_minor and currency required")
		return
	}
	id, err := h.svc.RecordSpend(r.Context(), middleware.OrgCtx(r.Context()), pid,
		req.AmountMinor, req.Currency, req.Memo)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"expense_id": id})
}

// SetBudget — POST /projects/{pid}/budget (org.projects.manage).
func (h *ProjectsHandler) SetBudget(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "project_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		AmountMinor int64  `json:"amount_minor"`
		Currency    string `json:"currency"`
		Note        string `json:"note"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.AmountMinor <= 0 || req.Currency == "" {
		httpx.WriteErr(w, 400, "validation_failed", "amount_minor and currency required")
		return
	}
	id, err := h.svc.SetBudget(r.Context(), middleware.OrgCtx(r.Context()), pid, req.AmountMinor, req.Currency, req.Note)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"budget_id": id})
}

// GetBudget — GET /projects/{pid}/budget: budget + live spend/remaining.
func (h *ProjectsHandler) GetBudget(w http.ResponseWriter, r *http.Request) {
	pid, err := uuid.Parse(chi.URLParam(r, "project_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	out, err := h.svc.Budget(r.Context(), middleware.OrgCtx(r.Context()), pid)
	if err != nil {
		projectErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}
