// Ref: oga9-engineering/sections/22-ai.md — §22 AI assistant routes.
package handler

import (
	"errors"
	"log"
	"net/http"

	"encoding/json"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jedafe/oga9/internal/ai"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

type AIHandler struct {
	svc   *ai.Service
	queue *asynq.Client
}

func NewAIHandler(s *ai.Service, queue *asynq.Client) *AIHandler {
	return &AIHandler{svc: s, queue: queue}
}

// enqueueAsync — §22 async path (Rule 8: fire-and-forget). Payload keys
// match the registered ai:answer / ai:generate_draft handlers.
func (h *AIHandler) enqueueAsync(w http.ResponseWriter, typ string, payload map[string]any) bool {
	if h.queue == nil {
		return false
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}
	if _, err := h.queue.Enqueue(asynq.NewTask(typ, body), asynq.Queue("default")); err != nil {
		return false
	}
	return true
}

func aiErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ai.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, ai.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, ai.ErrBadState):
		httpx.WriteErr(w, 409, "bad_generation_state", "draft already decided")
	case errors.Is(err, ai.ErrQuota):
		httpx.WriteErr(w, 429, "ai_quota_exceeded", "monthly ai limit reached — upgrade to continue")
	case errors.Is(err, ai.ErrNotProvider):
		httpx.WriteErr(w, 503, "ai_unavailable", "ai provider not configured")
	default:
		log.Printf("ai: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

type chatReq struct {
	Async          bool      `json:"async"` // enqueue ai:answer instead of answering inline
	ConversationID uuid.UUID `json:"conversation_id"`
	Persona        string    `json:"persona"`
	Question       string    `json:"question"`
}

func (h *AIHandler) Chat(w http.ResponseWriter, r *http.Request) {
	var req chatReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Question == "" {
		httpx.WriteErr(w, 400, "validation_failed", "question required")
		return
	}
	oc := middleware.OrgCtx(r.Context())
	if req.Async {
		if !h.enqueueAsync(w, "ai:answer", map[string]any{
			"org_id": oc.OrgID.String(), "user_id": oc.UserID.String(),
			"persona": req.Persona, "question": req.Question,
			"conversation_id": req.ConversationID.String()}) {
			httpx.WriteErr(w, 500, "internal_error", "enqueue failed")
			return
		}
		httpx.WriteData(w, 202, map[string]any{"queued": true})
		return
	}
	cid, mid, ans, err := h.svc.Chat(r.Context(), oc,
		req.ConversationID, req.Persona, req.Question)
	if err != nil {
		aiErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{
		"conversation_id": cid, "message_id": mid, "answer": ans})
}

func (h *AIHandler) Conversations(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.Conversations(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		aiErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *AIHandler) Messages(w http.ResponseWriter, r *http.Request) {
	cid, err := uuid.Parse(chi.URLParam(r, "conversation_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	rows, err := h.svc.Messages(r.Context(), middleware.OrgCtx(r.Context()), cid)
	if err != nil {
		aiErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

type draftReq struct {
	Async    bool   `json:"async"`     // enqueue ai:generate_draft instead of drafting inline
	TargetID string `json:"target_id"` // meeting for agenda/minutes apply
	Kind     string `json:"kind"`
	Prompt   string `json:"prompt"`
}

func (h *AIHandler) Draft(w http.ResponseWriter, r *http.Request) {
	var req draftReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Kind == "" {
		httpx.WriteErr(w, 400, "validation_failed", "kind required")
		return
	}
	tid, _ := uuid.Parse(req.TargetID)
	oc := middleware.OrgCtx(r.Context())
	if req.Async {
		if !h.enqueueAsync(w, "ai:generate_draft", map[string]any{
			"org_id": oc.OrgID, "user_id": oc.UserID, "kind": req.Kind,
			"prompt": req.Prompt, "target_id": tid}) {
			httpx.WriteErr(w, 500, "internal_error", "enqueue failed")
			return
		}
		httpx.WriteData(w, 202, map[string]any{"queued": true})
		return
	}
	id, err := h.svc.Draft(r.Context(), oc, req.Kind, req.Prompt, tid)
	if err != nil {
		aiErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"generation_id": id})
}

func (h *AIHandler) ListDrafts(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListGenerations(r.Context(), middleware.OrgCtx(r.Context()),
		r.URL.Query().Get("state"))
	if err != nil {
		aiErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

type aiReviewReq struct {
	Approve bool `json:"approve"`
}

func (h *AIHandler) Review(w http.ResponseWriter, r *http.Request) {
	gid, err := uuid.Parse(chi.URLParam(r, "generation_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req aiReviewReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "approve required")
		return
	}
	if err := h.svc.Review(r.Context(), middleware.OrgCtx(r.Context()), gid, req.Approve); err != nil {
		aiErr(w, err)
		return
	}
	state := "rejected"
	if req.Approve {
		state = "applied"
	}
	httpx.WriteData(w, 200, map[string]any{"state": state})
}

func (h *AIHandler) MyUsage(w http.ResponseWriter, r *http.Request) {
	row, err := h.svc.MyUsage(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		aiErr(w, err)
		return
	}
	httpx.WriteData(w, 200, row)
}

func (h *AIHandler) TenantUsage(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.TenantUsage(r.Context())
	if err != nil {
		aiErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}
