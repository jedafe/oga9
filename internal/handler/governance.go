// Ref: oga9-engineering/sections/16-governance.md — §16 governance routes.
package handler

import (
	"errors"
	"log"
	"net/http"

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
