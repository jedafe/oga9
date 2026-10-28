// Ref: oga9-engineering/sections/16-governance.md — §16 governance routes.
package handler

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/governance"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

type GovernanceHandler struct {
	svc *governance.Service
}

func NewGovernanceHandler(s *governance.Service) *GovernanceHandler {
	return &GovernanceHandler{svc: s}
}

func govErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, governance.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, governance.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, governance.ErrBadState):
		httpx.WriteErr(w, 409, "bad_motion_state", "motion is not in a votable state")
	case errors.Is(err, governance.ErrQuorum):
		httpx.WriteErr(w, 409, "quorum_not_met", "meeting quorum not met")
	case errors.Is(err, governance.ErrBadResolution):
		httpx.WriteErr(w, 422, "resolution_not_passed", "adoption requires a passed resolution")
	case errors.Is(err, governance.ErrNotCommittee):
		httpx.WriteErr(w, 422, "not_a_committee", "mandates apply to committee units only")
	case errors.Is(err, governance.ErrNoHearing):
		httpx.WriteErr(w, 409, "hearing_required", "a held hearing is required before resolution")
	case errors.Is(err, governance.ErrBadAppeal):
		httpx.WriteErr(w, 409, "appeal_not_allowed", "only resolved cases may be appealed")
	case errors.Is(err, governance.ErrNotSubject):
		httpx.WriteErr(w, 403, "not_the_subject", "only the case subject may appeal")
	default:
		log.Printf("governance: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

type motionReq struct {
	ActivityID string `json:"activity_id"`
	SeconderID string `json:"seconder_id"`
	Title      string `json:"title"`
	Body       string `json:"body"`
}

func (h *GovernanceHandler) ListMotions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListMotions(r.Context(), middleware.OrgCtx(r.Context()), r.URL.Query().Get("state"))
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *GovernanceHandler) CreateMotion(w http.ResponseWriter, r *http.Request) {
	var req motionReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Title == "" || req.Body == "" {
		httpx.WriteErr(w, 400, "validation_failed", "title and body required")
		return
	}
	aid, ok1 := httpx.OptUUID(w, req.ActivityID)
	sec, ok2 := httpx.OptUUID(w, req.SeconderID)
	if !ok1 || !ok2 {
		return
	}
	id, err := h.svc.CreateMotion(r.Context(), middleware.OrgCtx(r.Context()),
		aid, sec, req.Title, req.Body)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"motion_id": id})
}

type voteReq struct {
	Outcome      string `json:"outcome"` // passed|rejected|amended_passed
	VotesFor     int32  `json:"votes_for"`
	VotesAgainst int32  `json:"votes_against"`
	VotesAbstain int32  `json:"votes_abstain"`
}

func (h *GovernanceHandler) VoteMotion(w http.ResponseWriter, r *http.Request) {
	var req voteReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Outcome == "" {
		httpx.WriteErr(w, 400, "validation_failed", "outcome required")
		return
	}
	mid, perr := uuid.Parse(chi.URLParam(r, "motion_id"))
	if perr != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	id, err := h.svc.RecordVote(r.Context(), middleware.OrgCtx(r.Context()),
		mid, req.Outcome, req.VotesFor, req.VotesAgainst, req.VotesAbstain)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"resolution_id": id})
}

func (h *GovernanceHandler) ListResolutions(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListResolutions(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *GovernanceHandler) ListConstitution(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListConstitution(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

type constitutionReq struct {
	Body         string `json:"body"`
	ResolutionID string `json:"resolution_id"`
}

func (h *GovernanceHandler) AdoptConstitution(w http.ResponseWriter, r *http.Request) {
	var req constitutionReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Body == "" || req.ResolutionID == "" {
		httpx.WriteErr(w, 400, "validation_failed", "body and resolution_id required")
		return
	}
	rid, ok := httpx.OptUUID(w, req.ResolutionID)
	if !ok || rid == uuid.Nil {
		return
	}
	id, version, err := h.svc.AdoptConstitution(r.Context(), middleware.OrgCtx(r.Context()),
		req.Body, rid)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"constitution_version_id": id, "version": version})
}

func (h *GovernanceHandler) GetMandate(w http.ResponseWriter, r *http.Request) {
	uid, perr := uuid.Parse(chi.URLParam(r, "unit_id"))
	if perr != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	m, err := h.svc.GetMandate(r.Context(), middleware.OrgCtx(r.Context()), uid)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, m)
}

func (h *GovernanceHandler) ListMandates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListMandates(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

type mandateReq struct {
	Mandate          string `json:"mandate"`
	ReportsTo        string `json:"reports_to"`
	BudgetLimitMinor int64  `json:"budget_limit_minor"`
	Currency         string `json:"currency"`
	Active           bool   `json:"active"`
}

func (h *GovernanceHandler) UpsertMandate(w http.ResponseWriter, r *http.Request) {
	var req mandateReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Mandate == "" {
		httpx.WriteErr(w, 400, "validation_failed", "mandate required")
		return
	}
	uid, perr := uuid.Parse(chi.URLParam(r, "unit_id"))
	if perr != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	reportsTo, ok := httpx.OptUUID(w, req.ReportsTo)
	if !ok {
		return
	}
	if err := h.svc.UpsertMandate(r.Context(), middleware.OrgCtx(r.Context()),
		uid, req.Mandate, reportsTo, req.BudgetLimitMinor, req.Currency, req.Active); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"updated": true})
}

func (h *GovernanceHandler) Handover(w http.ResponseWriter, r *http.Request) {
	report, err := h.svc.Handover(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, report)
}

// ---------- §16 discipline casework ----------

type caseReq struct {
	SubjectID string `json:"subject_id"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Details   string `json:"details"`
}

type resolveReq struct {
	Sanction  string `json:"sanction"` // none|warning|fine|suspension
	FineMinor int64  `json:"fine_minor"`
	Currency  string `json:"currency"`
}

func (h *GovernanceHandler) ListCases(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListCases(r.Context(), middleware.OrgCtx(r.Context()), r.URL.Query().Get("state"))
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *GovernanceHandler) OpenCase(w http.ResponseWriter, r *http.Request) {
	var req caseReq
	if err := httpx.Decode(w, r, &req); err != nil || req.SubjectID == "" || req.Kind == "" || req.Title == "" {
		httpx.WriteErr(w, 400, "validation_failed", "subject_id, kind and title required")
		return
	}
	sid, err := uuid.Parse(req.SubjectID)
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed subject_id")
		return
	}
	id, err := h.svc.OpenCase(r.Context(), middleware.OrgCtx(r.Context()), sid, req.Kind, req.Title, req.Details)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"case_id": id})
}

func (h *GovernanceHandler) GetCase(w http.ResponseWriter, r *http.Request) {
	cid, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	c, err := h.svc.GetCase(r.Context(), middleware.OrgCtx(r.Context()), cid)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, c)
}

func (h *GovernanceHandler) AdvanceCase(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Action string `json:"action"` // investigate|dismiss|reopen
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Action == "" {
		httpx.WriteErr(w, 400, "validation_failed", "action required")
		return
	}
	cid, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.Advance(r.Context(), middleware.OrgCtx(r.Context()), cid, req.Action); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"updated": true})
}

func (h *GovernanceHandler) ScheduleHearing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ScheduledAt string `json:"scheduled_at"`
	}
	_ = httpx.Decode(w, r, &req) // scheduled_at is optional
	cid, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	at, _ := time.Parse(time.RFC3339, req.ScheduledAt)
	id, err := h.svc.ScheduleHearing(r.Context(), middleware.OrgCtx(r.Context()), cid, at)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"hearing_id": id})
}

// ListHearings — GET .../hearings: the case's hearing log.
func (h *GovernanceHandler) ListHearings(w http.ResponseWriter, r *http.Request) {
	cid, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	list, err := h.svc.ListHearings(r.Context(), middleware.OrgCtx(r.Context()), cid)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// CompleteHearing — POST .../hearings/{hid}/complete: marks the hearing
// held (outcome + notes); a case needs one held hearing before resolve.
func (h *GovernanceHandler) CompleteHearing(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Outcome string `json:"outcome"`
		Notes   string `json:"notes"`
	}
	_ = httpx.Decode(w, r, &req) // both optional
	cid, err1 := uuid.Parse(chi.URLParam(r, "case_id"))
	hid, err2 := uuid.Parse(chi.URLParam(r, "hearing_id"))
	if err1 != nil || err2 != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.CompleteHearing(r.Context(), middleware.OrgCtx(r.Context()),
		cid, hid, req.Outcome, req.Notes); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"held": true})
}

// ListAppeals — GET .../appeals: the case's appeals.
func (h *GovernanceHandler) ListAppeals(w http.ResponseWriter, r *http.Request) {
	cid, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	list, err := h.svc.ListAppeals(r.Context(), middleware.OrgCtx(r.Context()), cid)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *GovernanceHandler) ResolveCase(w http.ResponseWriter, r *http.Request) {
	var req resolveReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Sanction == "" {
		httpx.WriteErr(w, 400, "validation_failed", "sanction required")
		return
	}
	cid, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.Resolve(r.Context(), middleware.OrgCtx(r.Context()), cid,
		req.Sanction, req.FineMinor, req.Currency); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"resolved": true})
}

func (h *GovernanceHandler) FileAppeal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Grounds string `json:"grounds"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Grounds == "" {
		httpx.WriteErr(w, 400, "validation_failed", "grounds required")
		return
	}
	cid, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	id, err := h.svc.FileAppeal(r.Context(), middleware.OrgCtx(r.Context()), cid, req.Grounds)
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"appeal_id": id})
}

func (h *GovernanceHandler) DecideAppeal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Uphold bool `json:"uphold"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "malformed body")
		return
	}
	aid, err := uuid.Parse(chi.URLParam(r, "appeal_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.DecideAppeal(r.Context(), middleware.OrgCtx(r.Context()), aid, req.Uphold); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"decided": true})
}

func (h *GovernanceHandler) MyCases(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.MyCases(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// DebateMotion — POST /motions/{mid}/debate: marks a tabled motion as
// under debate (§16 state machine tabled → debated → voted).
func (h *GovernanceHandler) DebateMotion(w http.ResponseWriter, r *http.Request) {
	mid, err := uuid.Parse(chi.URLParam(r, "motion_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.DebateMotion(r.Context(), middleware.OrgCtx(r.Context()), mid); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"debated": true})
}

// WithdrawMotion — POST /motions/{mid}/withdraw: pulls a tabled/debated
// motion before a vote (§16 'withdrawn' state).
func (h *GovernanceHandler) WithdrawMotion(w http.ResponseWriter, r *http.Request) {
	mid, err := uuid.Parse(chi.URLParam(r, "motion_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.WithdrawMotion(r.Context(), middleware.OrgCtx(r.Context()), mid); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"withdrawn": true})
}

// CompleteHandover — POST /handover/{term_id}/complete: the incoming EXCO
// acknowledges the handover report → HandoverCompleted (idempotent).
func (h *GovernanceHandler) CompleteHandover(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "term_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.CompleteHandover(r.Context(), middleware.OrgCtx(r.Context()), tid); err != nil {
		govErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"completed": true})
}
