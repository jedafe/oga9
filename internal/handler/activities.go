// Ref: oga9-engineering/sections/13-events-meetings.md — events & meetings.
// Notice-window violations are warnings, not blocks (warn flag in create
// response); capacity is enforced at register (event_full).
package handler

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/activities"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tenant"
)

type ActivitiesHandler struct {
	svc *activities.Service
	org *tenant.Service
}

func NewActivitiesHandler(svc *activities.Service, org *tenant.Service) *ActivitiesHandler {
	return &ActivitiesHandler{svc: svc, org: org}
}

func actErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, activities.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "no such activity")
	case errors.Is(err, activities.ErrInvalidTransition):
		httpx.WriteErr(w, 409, "invalid_state_transition", "action not allowed in current state")
	case errors.Is(err, activities.ErrEventFull):
		httpx.WriteErr(w, 409, "event_full", "event is at capacity")
	case errors.Is(err, activities.ErrAlreadyRegistered):
		httpx.WriteErr(w, 409, "already_registered", "member is already registered")
	case errors.Is(err, activities.ErrGuestLimit):
		httpx.WriteErr(w, 409, "guest_limit", "guest registration cap reached for this event")
	case errors.Is(err, activities.ErrBadKind):
		httpx.WriteErr(w, 400, "invalid_kind", "kind must be meeting|event")
	case errors.Is(err, activities.ErrBadValue):
		httpx.WriteErr(w, 400, "validation_failed", "invalid field value")
	default:
		log.Printf("activities: %v", err) // detail stays server-side
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

func (h *ActivitiesHandler) create(w http.ResponseWriter, r *http.Request, kind string) {
	oc := middleware.OrgCtx(r.Context())
	var in activities.ActivityIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	in.Kind = kind
	id, warn, err := h.svc.CreateActivity(r.Context(), oc, in)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "notice_warning": warn})
}

func (h *ActivitiesHandler) Meetings(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, "meeting")
}
func (h *ActivitiesHandler) Events(w http.ResponseWriter, r *http.Request) {
	h.list(w, r, "event")
}
func (h *ActivitiesHandler) CreateMeeting(w http.ResponseWriter, r *http.Request) {
	h.create(w, r, "meeting")
}
func (h *ActivitiesHandler) CreateEvent(w http.ResponseWriter, r *http.Request) {
	h.create(w, r, "event")
}

func (h *ActivitiesHandler) list(w http.ResponseWriter, r *http.Request, kind string) {
	oc := middleware.OrgCtx(r.Context())
	var uid uuid.UUID
	if u := r.URL.Query().Get("unit"); u != "" {
		var err error
		if uid, err = uuid.Parse(u); err != nil {
			httpx.WriteErr(w, 400, "validation_failed", "unit must be a uuid")
			return
		}
	}
	list, err := h.svc.List(r.Context(), oc, kind, r.URL.Query().Get("status"), uid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *ActivitiesHandler) Get(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	a, err := h.svc.Get(r.Context(), oc, aid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, a)
}

// patch — shared PATCH body for meetings + events; clear_unit strips the
// unit scope, and a reschedule re-evaluates the notice warning.
func (h *ActivitiesHandler) patch(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	var req struct {
		activities.ActivityIn
		Status    *string `json:"status"`
		ClearUnit bool    `json:"clear_unit"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	warn, err := h.svc.PatchActivity(r.Context(), oc, aid, req.ActivityIn, req.Status, req.ClearUnit)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": aid, "notice_warning": warn})
}

func (h *ActivitiesHandler) PatchMeeting(w http.ResponseWriter, r *http.Request) {
	h.patch(w, r)
}
func (h *ActivitiesHandler) PatchEvent(w http.ResponseWriter, r *http.Request) {
	h.patch(w, r)
}

func (h *ActivitiesHandler) Calendar(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var uid uuid.UUID
	if u := r.URL.Query().Get("unit"); u != "" {
		var err error
		if uid, err = uuid.Parse(u); err != nil {
			httpx.WriteErr(w, 400, "validation_failed", "unit must be a uuid")
			return
		}
	}
	list, err := h.svc.OrgCalendar(r.Context(), oc, uid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *ActivitiesHandler) Attendance(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	var req struct {
		Marks []activities.MarkIn `json:"marks"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || len(req.Marks) == 0 {
		httpx.WriteErr(w, 400, "validation_failed", "marks required")
		return
	}
	if err := h.svc.MarkAttendance(r.Context(), oc, aid, req.Marks); err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"marked": len(req.Marks)})
}

func (h *ActivitiesHandler) DraftMinutes(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Body == "" {
		httpx.WriteErr(w, 400, "validation_failed", "body required")
		return
	}
	v, err := h.svc.DraftMinutes(r.Context(), oc, aid, req.Body)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"version": v})
}

func (h *ActivitiesHandler) ApproveMinutes(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	var req struct {
		MinutesID uuid.UUID `json:"minutes_id"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.MinutesID == uuid.Nil {
		httpx.WriteErr(w, 400, "validation_failed", "minutes_id required")
		return
	}
	if err := h.svc.ApproveMinutes(r.Context(), oc, aid, req.MinutesID); err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": req.MinutesID, "state": "approved"})
}

func (h *ActivitiesHandler) CreateItem(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	var req struct {
		AssigneeID uuid.UUID `json:"assignee_id"`
		Title      string    `json:"title"`
		DueDate    time.Time `json:"due_date"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Title == "" {
		httpx.WriteErr(w, 400, "validation_failed", "title required")
		return
	}
	id, err := h.svc.CreateActionItem(r.Context(), oc, aid, req.AssigneeID, req.Title, req.DueDate)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *ActivitiesHandler) Items(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	// §13: org.action_items.manage sees the whole sheet; everyone else
	// sees only their own items (view_own).
	var assignee uuid.UUID
	if !h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.action_items.manage") {
		assignee = oc.MembershipID
	}
	list, err := h.svc.ActionItems(r.Context(), oc, aid, r.URL.Query().Get("state"), assignee)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// PatchItem — PATCH /meetings/{aid}/action-items/{item_id}: field edits
// (manage) or {"state":"done"} self-complete (view_own via own assignee row).
func (h *ActivitiesHandler) PatchItem(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	itemID, err := uuid.Parse(chi.URLParam(r, "item_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "item_id must be a uuid")
		return
	}
	var req struct {
		State      string    `json:"state"`
		Title      string    `json:"title"`
		AssigneeID uuid.UUID `json:"assignee_id"`
		DueDate    time.Time `json:"due_date"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	manage := h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.action_items.manage")
	switch req.State {
	case "done":
		if err := h.svc.CompleteActionItem(r.Context(), oc, itemID, !manage); err != nil {
			actErr(w, err)
			return
		}
		httpx.WriteData(w, 200, map[string]any{"id": itemID, "state": "done"})
		return
	case "cancelled":
		if !manage {
			httpx.WriteErr(w, 403, "forbidden", "requires org.action_items.manage")
			return
		}
		if err := h.svc.CancelActionItem(r.Context(), oc, itemID); err != nil {
			actErr(w, err)
			return
		}
		httpx.WriteData(w, 200, map[string]any{"id": itemID, "state": "cancelled"})
		return
	case "":
	default:
		httpx.WriteErr(w, 400, "validation_failed", "state must be done|cancelled")
		return
	}
	if !manage {
		httpx.WriteErr(w, 403, "forbidden", "requires org.action_items.manage")
		return
	}
	if req.Title == "" && req.AssigneeID == uuid.Nil && req.DueDate.IsZero() {
		httpx.WriteErr(w, 400, "validation_failed", "nothing to update")
		return
	}
	if err := h.svc.PatchActionItem(r.Context(), oc, itemID, req.Title, req.AssigneeID, req.DueDate); err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": itemID})
}

func (h *ActivitiesHandler) Register(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	var req struct {
		GuestName    string `json:"guest_name"`
		GuestContact string `json:"guest_contact"`
	}
	_ = httpx.Decode(w, r, &req)
	mid := oc.MembershipID
	if req.GuestName != "" {
		mid = uuid.Nil // guest registration
	}
	id, err := h.svc.RegisterEvent(r.Context(), oc, aid, mid, req.GuestName, req.GuestContact)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

// CheckIn — members check in by membership_id, guests by registration_id.
func (h *ActivitiesHandler) CheckIn(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	var req struct {
		MembershipID   uuid.UUID `json:"membership_id"`
		RegistrationID uuid.UUID `json:"registration_id"`
	}
	if err := httpx.Decode(w, r, &req); err != nil ||
		(req.MembershipID == uuid.Nil && req.RegistrationID == uuid.Nil) {
		httpx.WriteErr(w, 400, "validation_failed", "membership_id or registration_id required")
		return
	}
	if err := h.svc.CheckIn(r.Context(), oc, aid, req.MembershipID, req.RegistrationID); err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"status": "checked_in"})
}

func (h *ActivitiesHandler) Registrations(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	list, err := h.svc.Registrations(r.Context(), oc, aid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// Minutes — GET /meetings/{aid}/minutes: the versioned minutes list.
func (h *ActivitiesHandler) Minutes(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	list, err := h.svc.Minutes(r.Context(), oc, aid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// Agenda — GET /meetings/{aid}/agenda: the published agenda items.
func (h *ActivitiesHandler) Agenda(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	items, err := h.svc.GetAgenda(r.Context(), oc, aid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, items)
}

// Sheet — GET /meetings/{aid}/attendance: the marked sheet for the UI.
func (h *ActivitiesHandler) Sheet(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	list, err := h.svc.AttendanceSheet(r.Context(), oc, aid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// Quorum — POST /meetings/{aid}/quorum: evaluate present+late vs the
// meeting's quorum_percent (meetings.manage; emits QuorumEvaluated).
func (h *ActivitiesHandler) Quorum(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "activity_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "activity_id must be a uuid")
		return
	}
	met, pct, err := h.svc.EvaluateQuorum(r.Context(), oc, aid)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"met": met, "percent": pct})
}

// Recurrences — GET /orgs/{id}/recurrences: the org's active series.
func (h *ActivitiesHandler) Recurrences(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.Recurrences(r.Context(), oc)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// CreateRecurrence — POST /orgs/{id}/recurrences: register a series the
// daily generate_recurrences sweep materializes.
func (h *ActivitiesHandler) CreateRecurrence(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var in activities.RecurrenceIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.CreateRecurrence(r.Context(), oc, in)
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *ActivitiesHandler) MyCalendar(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.MyCalendar(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *ActivitiesHandler) MyItems(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.MyActionItems(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		actErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}
