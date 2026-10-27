// Ref: oga9-engineering/sections/21-elections.md — §21 election routes.
package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/elections"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

type ElectionsHandler struct {
	svc *elections.Service
}

func NewElectionsHandler(s *elections.Service) *ElectionsHandler {
	return &ElectionsHandler{svc: s}
}

func electionErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, elections.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, elections.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, elections.ErrBadState):
		httpx.WriteErr(w, 409, "bad_election_state", "election is not in a state for that action")
	case errors.Is(err, elections.ErrNotEligible):
		httpx.WriteErr(w, 422, "not_eligible", "member does not meet eligibility rules")
	case errors.Is(err, elections.ErrAlreadyVoted):
		httpx.WriteErr(w, 409, "already_voted", "ballot already cast for this member")
	case errors.Is(err, elections.ErrDuplicate):
		httpx.WriteErr(w, 409, "already_nominated", "member already nominated for that position")
	default:
		log.Printf("elections: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

func electionID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "election_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return uuid.Nil, false
	}
	return id, true
}

type electionReq struct {
	TermID      uuid.UUID       `json:"term_id"`
	Title       string          `json:"title"`
	Eligibility json.RawMessage `json:"eligibility"`
}

func (h *ElectionsHandler) List(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.List(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *ElectionsHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req electionReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Title == "" || req.TermID == uuid.Nil {
		httpx.WriteErr(w, 400, "validation_failed", "term_id and title required")
		return
	}
	if len(req.Eligibility) == 0 {
		req.Eligibility = json.RawMessage("{}")
	}
	id, err := h.svc.Create(r.Context(), middleware.OrgCtx(r.Context()),
		req.TermID, req.Title, req.Eligibility)
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"election_id": id})
}

func (h *ElectionsHandler) Get(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	e, err := h.svc.Get(r.Context(), middleware.OrgCtx(r.Context()), eid)
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, e)
}

type scheduleReq struct {
	NominationsOpen time.Time `json:"nominations_open"`
	VotingOpens     time.Time `json:"voting_opens"`
	VotingCloses    time.Time `json:"voting_closes"`
	Action          string    `json:"action"` // begin_nominations|open_voting|close_voting
}

func (h *ElectionsHandler) Patch(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	var req scheduleReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "malformed body")
		return
	}
	oc := middleware.OrgCtx(r.Context())
	var err error
	switch req.Action {
	case "begin_nominations":
		err = h.svc.BeginNominations(r.Context(), oc, eid)
	case "open_voting":
		err = h.svc.OpenVoting(r.Context(), oc, eid)
	case "close_voting":
		err = h.svc.CloseVoting(r.Context(), oc, eid)
	default:
		err = h.svc.Schedule(r.Context(), oc, eid,
			req.NominationsOpen, req.VotingOpens, req.VotingCloses)
	}
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"done": true})
}

type nominateReq struct {
	PositionID  uuid.UUID `json:"position_id"`
	MemberID    uuid.UUID `json:"member_id"`
	ScopeUnitID uuid.UUID `json:"scope_unit_id"` // scoped contest (§21)
	Manifesto   string    `json:"manifesto"`
}

func (h *ElectionsHandler) Nominate(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	var req nominateReq
	if err := httpx.Decode(w, r, &req); err != nil || req.PositionID == uuid.Nil || req.MemberID == uuid.Nil {
		httpx.WriteErr(w, 400, "validation_failed", "position_id and member_id required")
		return
	}
	id, err := h.svc.Nominate(r.Context(), middleware.OrgCtx(r.Context()), eid,
		req.PositionID, req.MemberID, req.ScopeUnitID, req.Manifesto)
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"candidate_id": id})
}

type vetReq struct {
	Decision string `json:"decision"` // vetted|disqualified|withdrawn
	Reason   string `json:"reason"`
}

func (h *ElectionsHandler) Vet(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	cid, err := uuid.Parse(chi.URLParam(r, "candidate_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req vetReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Decision == "" {
		httpx.WriteErr(w, 400, "validation_failed", "decision required")
		return
	}
	if err := h.svc.Vet(r.Context(), middleware.OrgCtx(r.Context()), eid, cid, req.Decision, req.Reason); err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": req.Decision})
}

func (h *ElectionsHandler) Ballot(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Ballot(r.Context(), middleware.OrgCtx(r.Context()), eid)
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

type electionVoteReq struct {
	Choices []elections.VoteChoice `json:"choices"`
}

func (h *ElectionsHandler) Vote(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	var req electionVoteReq
	if err := httpx.Decode(w, r, &req); err != nil || len(req.Choices) == 0 {
		httpx.WriteErr(w, 400, "validation_failed", "choices required")
		return
	}
	if err := h.svc.Vote(r.Context(), middleware.OrgCtx(r.Context()), eid, req.Choices); err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"cast": true})
}

func (h *ElectionsHandler) Results(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	out, err := h.svc.Results(r.Context(), middleware.OrgCtx(r.Context()), eid)
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

func (h *ElectionsHandler) Ratify(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Ratify(r.Context(), middleware.OrgCtx(r.Context()), eid); err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "ratified"})
}

type annulReq struct {
	Reason string `json:"reason"`
}

func (h *ElectionsHandler) Annul(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	var req annulReq
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.Annul(r.Context(), middleware.OrgCtx(r.Context()), eid, req.Reason); err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "annulled"})
}

func (h *ElectionsHandler) Audit(w http.ResponseWriter, r *http.Request) {
	eid, ok := electionID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.AuditTrail(r.Context(), middleware.OrgCtx(r.Context()), eid)
	if err != nil {
		electionErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}
