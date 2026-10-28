// Ref: oga9-engineering/sections/26-polls-surveys.md, 27-volunteers.md
package handler

import (
	"errors"
	"log"
	"net/http"

	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/activities"
	"github.com/jedafe/oga9/internal/comms"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

func pollErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, comms.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "no such poll/survey")
	case errors.Is(err, comms.ErrAlreadyVoted), errors.Is(err, comms.ErrAlreadyRespond):
		httpx.WriteErr(w, 409, "already_voted", "you have already responded")
	case errors.Is(err, comms.ErrNotInAudience):
		httpx.WriteErr(w, 403, "not_in_audience", "this poll/survey is not open to you")
	case errors.Is(err, comms.ErrNotOpen):
		httpx.WriteErr(w, 409, "invalid_state_transition", "not open")
	case errors.Is(err, comms.ErrBadValue):
		httpx.WriteErr(w, 400, "validation_failed", "invalid field value")
	default:
		log.Printf("polls: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

// ---------- polls (CommsHandler) ----------

func (h *CommsHandler) Polls(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListPolls(r.Context(), oc, r.URL.Query().Get("state"))
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *CommsHandler) CreatePoll(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in comms.PollIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.CreatePoll(r.Context(), oc, in)
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *CommsHandler) GetPoll(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "poll_id")
	if !ok {
		return
	}
	p, opts, voted, err := h.svc.GetPoll(r.Context(), oc, pid)
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"poll": p, "options": opts, "my_ballot": voted})
}

func (h *CommsHandler) OpenPoll(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "poll_id")
	if !ok {
		return
	}
	if err := h.svc.OpenPoll(r.Context(), oc, pid); err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "open"})
}

func (h *CommsHandler) Vote(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "poll_id")
	if !ok {
		return
	}
	var in struct {
		OptionIDs []uuid.UUID `json:"option_ids"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || len(in.OptionIDs) == 0 {
		httpx.WriteErr(w, 400, "validation_failed", "option_ids required")
		return
	}
	if err := h.svc.Vote(r.Context(), oc, pid, in.OptionIDs); err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"voted": true})
}

func (h *CommsHandler) ClosePoll(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "poll_id")
	if !ok {
		return
	}
	if err := h.svc.ClosePoll(r.Context(), oc, pid); err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "closed"})
}

func (h *CommsHandler) PollResults(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	pid, ok := pathUUID(w, r, "poll_id")
	if !ok {
		return
	}
	out, err := h.svc.PollResults(r.Context(), oc, pid)
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

// ---------- surveys (CommsHandler) ----------

func (h *CommsHandler) Surveys(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.ListSurveys(r.Context(), oc, r.URL.Query().Get("state"))
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *CommsHandler) CreateSurvey(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in comms.SurveyIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.CreateSurvey(r.Context(), oc, in)
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *CommsHandler) GetSurvey(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "survey_id")
	if !ok {
		return
	}
	sv, mine, err := h.svc.GetSurvey(r.Context(), oc, sid)
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"survey": sv, "my_submitted": mine})
}

func (h *CommsHandler) OpenSurvey(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "survey_id")
	if !ok {
		return
	}
	if err := h.svc.OpenSurvey(r.Context(), oc, sid); err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "open"})
}

func (h *CommsHandler) Respond(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "survey_id")
	if !ok {
		return
	}
	var in struct {
		Answers map[string]any `json:"answers"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "answers required")
		return
	}
	if err := h.svc.Respond(r.Context(), oc, sid, in.Answers); err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"submitted": true})
}

func (h *CommsHandler) CloseSurvey(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "survey_id")
	if !ok {
		return
	}
	if err := h.svc.CloseSurvey(r.Context(), oc, sid); err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "closed"})
}

func (h *CommsHandler) SurveyResults(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "survey_id")
	if !ok {
		return
	}
	out, err := h.svc.SurveyResults(r.Context(), oc, sid)
	if err != nil {
		pollErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

// ---------- volunteers (ActivitiesHandler) ----------

func volErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, activities.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "no such volunteer role/signup")
	case errors.Is(err, activities.ErrRoleFull):
		httpx.WriteErr(w, 409, "role_full", "role is at capacity — signup queued as standby")
	case errors.Is(err, activities.ErrNotEligible):
		httpx.WriteErr(w, 403, "not_eligible", "role is restricted to a unit you are not in")
	case errors.Is(err, activities.ErrAlreadySigned):
		httpx.WriteErr(w, 409, "already_signed_up", "you are already signed up for this role")
	case errors.Is(err, activities.ErrInvalidTransition):
		httpx.WriteErr(w, 409, "invalid_state_transition", "action not allowed in current state")
	case errors.Is(err, activities.ErrBadValue):
		httpx.WriteErr(w, 400, "validation_failed", "invalid field value")
	default:
		log.Printf("volunteers: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

func (h *ActivitiesHandler) VolunteerRoles(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, ok := pathUUID(w, r, "activity_id")
	if !ok {
		return
	}
	list, err := h.svc.VolunteerRoles(r.Context(), oc, aid)
	if err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *ActivitiesHandler) CreateVolunteerRole(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, ok := pathUUID(w, r, "activity_id")
	if !ok {
		return
	}
	var in activities.VolunteerRoleIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.CreateVolunteerRole(r.Context(), oc, aid, in)
	if err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *ActivitiesHandler) PatchVolunteerRole(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	rid, ok := pathUUID(w, r, "role_id")
	if !ok {
		return
	}
	var in struct {
		activities.VolunteerRoleIn
		ClearUnit bool `json:"clear_unit"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if err := h.svc.PatchVolunteerRole(r.Context(), oc, rid, in.VolunteerRoleIn, in.ClearUnit); err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": rid})
}

func (h *ActivitiesHandler) DeleteVolunteerRole(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	rid, ok := pathUUID(w, r, "role_id")
	if !ok {
		return
	}
	if err := h.svc.DeleteVolunteerRole(r.Context(), oc, rid); err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"deleted": true})
}

func (h *ActivitiesHandler) VolunteerSignup(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, ok := pathUUID(w, r, "activity_id")
	if !ok {
		return
	}
	rid, ok := pathUUID(w, r, "role_id")
	if !ok {
		return
	}
	var in struct {
		Note string `json:"note"`
	}
	_ = httpx.Decode(w, r, &in)
	id, err := h.svc.Signup(r.Context(), oc, aid, rid, in.Note)
	if err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *ActivitiesHandler) WithdrawSignup(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "signup_id")
	if !ok {
		return
	}
	if err := h.svc.Withdraw(r.Context(), oc, sid); err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "cancelled"})
}

func (h *ActivitiesHandler) ConfirmSignup(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "signup_id")
	if !ok {
		return
	}
	if err := h.svc.ConfirmSignup(r.Context(), oc, sid); err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "confirmed"})
}

// MarkSignup — officer marks completed|no_show post-activity (§27).
func (h *ActivitiesHandler) MarkSignup(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	sid, ok := pathUUID(w, r, "signup_id")
	if !ok {
		return
	}
	var in struct {
		State string `json:"state"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "state required")
		return
	}
	if err := h.svc.MarkSignup(r.Context(), oc, sid, in.State); err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": in.State})
}

func (h *ActivitiesHandler) VolunteerRoster(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, ok := pathUUID(w, r, "activity_id")
	if !ok {
		return
	}
	list, err := h.svc.Roster(r.Context(), oc, aid)
	if err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *ActivitiesHandler) MyVolunteerSignups(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r.Context())
	list, err := h.svc.MyVolunteerSignups(r.Context(), uid)
	if err != nil {
		volErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}
