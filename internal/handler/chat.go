// Ref: oga9-engineering/sections/24-realtime-chat-voice.md — chat routes.
package handler

import (
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tenant"
)

type ChatHandler struct {
	svc    *chat.Service
	orgSvc *tenant.Service // moderate check via org permission nodes
}

func NewChatHandler(svc *chat.Service, orgSvc *tenant.Service) *ChatHandler {
	return &ChatHandler{svc: svc, orgSvc: orgSvc}
}

func chatErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, chat.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "no such record")
	case errors.Is(err, chat.ErrNotMember):
		httpx.WriteErr(w, 403, "forbidden", "not a channel member")
	case errors.Is(err, chat.ErrMuted):
		httpx.WriteErr(w, 403, "forbidden", "channel muted")
	case errors.Is(err, chat.ErrVoiceUnavailable):
		httpx.WriteErr(w, 503, "service_unavailable", "voice provider is not configured")
	case errors.Is(err, chat.ErrForbidden):
		httpx.WriteErr(w, 403, "forbidden", "insufficient permission")
	case errors.Is(err, chat.ErrEditWindow):
		httpx.WriteErr(w, 409, "edit_window_expired", "edits are allowed for a limited window")
	case errors.Is(err, chat.ErrBadValue):
		httpx.WriteErr(w, 400, "validation_failed", "invalid field value")
	default:
		log.Printf("chat: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

func (h *ChatHandler) Channels(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.Channels(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *ChatHandler) CreateChannel(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var req struct {
		Kind   string    `json:"kind"`
		Name   string    `json:"name"`
		UnitID uuid.UUID `json:"unit_id"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.CreateChannel(r.Context(), oc, req.Kind, req.Name, req.UnitID)
	if err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *ChatHandler) Messages(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var before *time.Time
	if b := r.URL.Query().Get("before"); b != "" {
		if t, err := time.Parse(time.RFC3339Nano, b); err == nil {
			before = &t
		}
	}
	list, err := h.svc.Messages(r.Context(), oc, cid, before)
	if err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *ChatHandler) PostMessage(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Kind       string    `json:"kind"`
		Body       string    `json:"body"`
		MediaID    uuid.UUID `json:"media_id"`
		ReplyToID  uuid.UUID `json:"reply_to_id"`
		VoiceDurMs int       `json:"voice_duration_ms"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	id, err := h.svc.PostMessage(r.Context(), oc, cid, req.Kind, req.Body, req.MediaID, req.ReplyToID, req.VoiceDurMs)
	if err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

// Delete — own message always; `org.chat.moderate` allows others.
func (h *ChatHandler) DeleteMessage(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	mid, err := uuid.Parse(chi.URLParam(r, "message_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	mod := h.orgSvc.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.chat.moderate")
	if err := h.svc.Delete(r.Context(), oc, cid, mid, mod); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": mid})
}

// EditMessage — PATCH body edit within the org's edit window; own only.
func (h *ChatHandler) EditMessage(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	mid, err := uuid.Parse(chi.URLParam(r, "message_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Body string `json:"body"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if err := h.svc.EditMessage(r.Context(), oc, cid, mid, req.Body); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": mid})
}

func (h *ChatHandler) MarkRead(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		MessageID uuid.UUID `json:"message_id"`
	}
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.MarkRead(r.Context(), oc, cid, req.MessageID); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"ok": true})
}

// AddMember — POST /channels/{cid}/members (channels.manage): seats a
// member on a custom/dm/committee channel; derived kinds refuse.
func (h *ChatHandler) AddMember(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		MembershipID uuid.UUID `json:"membership_id"`
		Role         string    `json:"role"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.MembershipID == uuid.Nil {
		httpx.WriteErr(w, 400, "validation_failed", "membership_id required")
		return
	}
	if err := h.svc.AddMember(r.Context(), oc, cid, req.MembershipID, req.Role); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"channel_id": cid, "membership_id": req.MembershipID})
}

func (h *ChatHandler) Mute(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Until *time.Time `json:"until"`
	}
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.Mute(r.Context(), oc, cid, req.Until); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"ok": true})
}

// ---------- voice rooms (§24, LiveKit) ----------

// StartRoom — POST /orgs/{id}/voice-rooms {title, channel_id, activity_id}.
func (h *ChatHandler) StartRoom(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var req struct {
		Title      string    `json:"title"`
		ChannelID  uuid.UUID `json:"channel_id"`
		ActivityID uuid.UUID `json:"activity_id"`
	}
	_ = httpx.Decode(w, r, &req)
	id, err := h.svc.StartRoom(r.Context(), oc, req.Title, req.ChannelID, req.ActivityID)
	if err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

// JoinRoom — returns the room name + a short-lived LiveKit access token.
func (h *ChatHandler) JoinRoom(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	rid, err := uuid.Parse(chi.URLParam(r, "room_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	name, token, url, err := h.svc.JoinRoom(r.Context(), oc, rid)
	if err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{
		"room": name, "token": token, "provider_url": url})
}

func (h *ChatHandler) LeaveRoom(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	rid, err := uuid.Parse(chi.URLParam(r, "room_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.LeaveRoom(r.Context(), oc, rid); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"room_id": rid})
}

// EndRoom — starter or `org.chat.moderate`.
func (h *ChatHandler) EndRoom(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	rid, err := uuid.Parse(chi.URLParam(r, "room_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	mod := h.orgSvc.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.chat.moderate")
	if err := h.svc.EndRoom(r.Context(), oc, rid, mod); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"room_id": rid, "state": "ended"})
}

func (h *ChatHandler) LiveRooms(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.LiveRooms(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// Archive — POST /channels/{cid}/archive (channels.manage): retires a
// custom/dm channel; derived kinds refuse (they end with their source).
func (h *ChatHandler) Archive(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.ArchiveChannel(r.Context(), oc, cid); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"channel_id": cid, "state": "archived"})
}

// RemoveMember — DELETE /channels/{cid}/members/{mid}: officer removal
// (channels.manage) or self-leave on custom/dm channels.
func (h *ChatHandler) RemoveMember(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	cid, err := uuid.Parse(chi.URLParam(r, "channel_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	mid, err := uuid.Parse(chi.URLParam(r, "member_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if mid != oc.MembershipID &&
		!h.orgSvc.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.chat.channels.manage") {
		chatErr(w, chat.ErrForbidden)
		return
	}
	if err := h.svc.RemoveMember(r.Context(), oc, cid, mid); err != nil {
		chatErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"channel_id": cid, "membership_id": mid})
}
