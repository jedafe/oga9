// Ref: oga9-engineering/sections/17-welfare.md — §17 welfare routes.
package handler

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/welfare"
)

type WelfareHandler struct {
	svc *welfare.Service
}

func NewWelfareHandler(s *welfare.Service) *WelfareHandler {
	return &WelfareHandler{svc: s}
}

func welfareErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, welfare.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, welfare.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, welfare.ErrBadState):
		httpx.WriteErr(w, 409, "bad_case_state", "case is not in a state for that action")
	case errors.Is(err, welfare.ErrIneligible):
		httpx.WriteErr(w, 422, "welfare_ineligible", "member does not meet eligibility rules")
	case errors.Is(err, welfare.ErrDuplicate):
		httpx.WriteErr(w, 409, "already_decided", "approver has already recorded a decision")
	default:
		log.Printf("welfare: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

func caseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "case_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return uuid.Nil, false
	}
	return id, true
}

type welfareCaseReq struct {
	Kind                 string `json:"kind"`
	Title                string `json:"title"`
	Details              string `json:"details"`
	RequestedAmountMinor int64  `json:"requested_amount_minor"`
	Currency             string `json:"currency"`
}

func (h *WelfareHandler) ListCases(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListCases(r.Context(), middleware.OrgCtx(r.Context()), r.URL.Query().Get("state"))
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *WelfareHandler) CreateCase(w http.ResponseWriter, r *http.Request) {
	var req welfareCaseReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Kind == "" || req.Title == "" {
		httpx.WriteErr(w, 400, "validation_failed", "kind and title required")
		return
	}
	id, err := h.svc.CreateCase(r.Context(), middleware.OrgCtx(r.Context()),
		req.Kind, req.Title, req.Details, req.RequestedAmountMinor, req.Currency)
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"case_id": id})
}

func (h *WelfareHandler) GetCase(w http.ResponseWriter, r *http.Request) {
	cid, ok := caseID(w, r)
	if !ok {
		return
	}
	c, err := h.svc.GetCase(r.Context(), middleware.OrgCtx(r.Context()), cid)
	if err != nil {
		welfareErr(w, err)
		return
	}
	hist, err := h.svc.CaseHistory(r.Context(), middleware.OrgCtx(r.Context()), cid)
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"case": c, "history": hist})
}

type reviewReq struct {
	Note string `json:"note"`
}

func (h *WelfareHandler) ReviewCase(w http.ResponseWriter, r *http.Request) {
	cid, ok := caseID(w, r)
	if !ok {
		return
	}
	var req reviewReq
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.Review(r.Context(), middleware.OrgCtx(r.Context()), cid, req.Note); err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "reviewing"})
}

type decisionReq struct {
	Approve             bool   `json:"approve"`
	ApprovedAmountMinor int64  `json:"approved_amount_minor"`
	Currency            string `json:"currency"`
	Note                string `json:"note"`
}

func (h *WelfareHandler) DecideCase(w http.ResponseWriter, r *http.Request) {
	cid, ok := caseID(w, r)
	if !ok {
		return
	}
	var req decisionReq
	if err := httpx.Decode(w, r, &req); err != nil {
		return
	}
	approved, err := h.svc.Decide(r.Context(), middleware.OrgCtx(r.Context()), cid,
		req.Approve, req.ApprovedAmountMinor, req.Currency, req.Note)
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"approved": approved})
}

type disburseReq struct {
	Kind        string `json:"kind"` // cash|item|service|visit
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

func (h *WelfareHandler) DisburseCase(w http.ResponseWriter, r *http.Request) {
	cid, ok := caseID(w, r)
	if !ok {
		return
	}
	var req disburseReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Kind == "" {
		httpx.WriteErr(w, 400, "validation_failed", "kind required")
		return
	}
	id, err := h.svc.Disburse(r.Context(), middleware.OrgCtx(r.Context()), cid,
		req.Kind, req.AmountMinor, req.Currency)
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"benefit_id": id})
}

type visitReq struct {
	ScheduledAt string   `json:"scheduled_at"`
	Delegates   []string `json:"delegates"`
}

func (h *WelfareHandler) ScheduleVisit(w http.ResponseWriter, r *http.Request) {
	cid, ok := caseID(w, r)
	if !ok {
		return
	}
	var req visitReq
	if err := httpx.Decode(w, r, &req); err != nil {
		return
	}
	var at time.Time
	if req.ScheduledAt != "" {
		t, err := time.Parse(time.RFC3339, req.ScheduledAt)
		if err != nil {
			httpx.WriteErr(w, 400, "validation_failed", "scheduled_at must be RFC3339")
			return
		}
		at = t
	}
	var delegates []uuid.UUID
	for _, d := range req.Delegates {
		if id, err := uuid.Parse(d); err == nil {
			delegates = append(delegates, id)
		}
	}
	vid, err := h.svc.ScheduleVisit(r.Context(), middleware.OrgCtx(r.Context()), cid, at, delegates)
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"visit_id": vid})
}

func (h *WelfareHandler) CompleteVisit(w http.ResponseWriter, r *http.Request) {
	cid, ok := caseID(w, r)
	if !ok {
		return
	}
	visitID, err := uuid.Parse(chi.URLParam(r, "visit_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req reviewReq
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.CompleteVisit(r.Context(), middleware.OrgCtx(r.Context()), cid, visitID, req.Note); err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"completed": true})
}

func (h *WelfareHandler) CloseCase(w http.ResponseWriter, r *http.Request) {
	cid, ok := caseID(w, r)
	if !ok {
		return
	}
	var req reviewReq
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.CloseCase(r.Context(), middleware.OrgCtx(r.Context()), cid, req.Note); err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "closed"})
}

func (h *WelfareHandler) Policies(w http.ResponseWriter, r *http.Request) {
	pol, err := h.svc.Policies(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, pol)
}

// MyCases — /me/welfare: the member's own cases across orgs.
func (h *WelfareHandler) MyCases(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.MyCases(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		welfareErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}
