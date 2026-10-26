// Ref: oga9-engineering/sections/14-comms-documents.md — comms routes.
package handler

import (
	"errors"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jedafe/oga9/internal/comms"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tasks"
	"github.com/jedafe/oga9/internal/tenant"
)

type CommsHandler struct {
	svc   *comms.Service
	org   *tenant.Service // doc-visibility manage check via org nodes
	queue *asynq.Client
}

func NewCommsHandler(svc *comms.Service, queue *asynq.Client, org *tenant.Service) *CommsHandler {
	return &CommsHandler{svc: svc, org: org, queue: queue}
}

func commsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, comms.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "no such record")
	case errors.Is(err, comms.ErrTooLarge):
		httpx.WriteErr(w, 413, "upload_too_large", "file exceeds the plan storage limit")
	case errors.Is(err, comms.ErrBadValue):
		httpx.WriteErr(w, 400, "validation_failed", "invalid field value")
	default:
		log.Printf("comms: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

func (h *CommsHandler) Announcements(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	list, err := h.svc.List(r.Context(), oc, r.URL.Query().Get("state"))
	if err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *CommsHandler) CreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var req struct {
		Title    string         `json:"title"`
		Body     string         `json:"body"`
		Draft    bool           `json:"draft"`
		Audience comms.Audience `json:"audience"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Title == "" || req.Body == "" {
		httpx.WriteErr(w, 400, "validation_failed", "title and body required")
		return
	}
	id, err := h.svc.Publish(r.Context(), oc, req.Title, req.Body, req.Audience, req.Draft)
	if err != nil {
		commsErr(w, err)
		return
	}
	if !req.Draft {
		// Rule 8: fan-out is fire-and-forget — enqueue, never block.
		if t, err := tasks.NewDispatchTask(id); err == nil {
			_, _ = h.queue.EnqueueContext(r.Context(), t)
		}
	}
	httpx.WriteData(w, 201, map[string]any{"id": id, "state": map[bool]string{true: "draft", false: "published"}[req.Draft]})
}

// PublishAnnouncement — POST /announcements/{aid}/publish (announce):
// draft → published; the dispatch fan-out enqueues exactly like an
// immediate publish (Rule 8 fire-and-forget).
func (h *CommsHandler) PublishAnnouncement(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "announcement_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.PublishDraft(r.Context(), oc, aid); err != nil {
		commsErr(w, err)
		return
	}
	if t, err := tasks.NewDispatchTask(aid); err == nil {
		_, _ = h.queue.EnqueueContext(r.Context(), t)
	}
	httpx.WriteData(w, 200, map[string]any{"id": aid, "state": "published"})
}

func (h *CommsHandler) RetractAnnouncement(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	aid, err := uuid.Parse(chi.URLParam(r, "announcement_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.Retract(r.Context(), oc, aid); err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": aid, "state": "retracted"})
}

func (h *CommsHandler) MyNotifications(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.MyNotifications(r.Context(), middleware.UserID(r.Context()),
		r.URL.Query().Get("unread") == "true")
	if err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *CommsHandler) ReadNotification(w http.ResponseWriter, r *http.Request) {
	nid, err := uuid.Parse(chi.URLParam(r, "notification_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.MarkRead(r.Context(), middleware.UserID(r.Context()), nid); err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": nid})
}

func (h *CommsHandler) GetPrefs(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.MyPreferences(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *CommsHandler) SetPrefs(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Kind    string `json:"kind"`
		Channel string `json:"channel"`
		Enabled bool   `json:"enabled"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if !comms.Kinds[req.Kind] {
		httpx.WriteErr(w, 400, "validation_failed", "unknown notification kind")
		return
	}
	if err := h.svc.SetPreference(r.Context(), middleware.UserID(r.Context()), req.Kind, req.Channel, req.Enabled); err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"kind": req.Kind, "channel": req.Channel, "enabled": req.Enabled})
}

// ---------- documents (§14) ----------

// UploadDocument — POST /orgs/{id}/documents (multipart file + kind,
// title, visibility, unit_id). Enqueues documents:process_upload.
func (h *CommsHandler) UploadDocument(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "multipart body expected")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "file required")
		return
	}
	defer f.Close()
	uid, err := uuid.Parse(r.FormValue("unit_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	id, err := h.svc.Upload(r.Context(), oc, r.FormValue("kind"),
		r.FormValue("title"), r.FormValue("visibility"), uid, f)
	if err != nil {
		commsErr(w, err)
		return
	}
	if t, err := tasks.NewDocProcessTask(id); err == nil {
		_, _ = h.queue.EnqueueContext(r.Context(), t)
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *CommsHandler) Documents(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	manage := h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.documents.publish")
	list, err := h.svc.Documents(r.Context(), oc, r.URL.Query().Get("kind"), manage)
	if err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *CommsHandler) DownloadDocument(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, err := uuid.Parse(chi.URLParam(r, "document_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	manage := h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.documents.publish")
	rc, d, err := h.svc.Download(r.Context(), oc, did, manage)
	if err != nil {
		commsErr(w, err)
		return
	}
	defer rc.Close()
	fname := strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' {
			return -1
		}
		return r
	}, d.Title)
	w.Header().Set("Content-Disposition", `attachment; filename="`+fname+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, rc)
}

func (h *CommsHandler) PatchDocument(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, err := uuid.Parse(chi.URLParam(r, "document_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Title      *string   `json:"title"`
		Visibility *string   `json:"visibility"`
		ClearUnit  bool      `json:"clear_unit"`
		UnitID     uuid.UUID `json:"unit_id"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if err := h.svc.PatchDocument(r.Context(), oc, did, req.Title, req.Visibility, req.ClearUnit, req.UnitID); err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": did})
}

func (h *CommsHandler) ArchiveDocument(w http.ResponseWriter, r *http.Request) {
	did, err := uuid.Parse(chi.URLParam(r, "document_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.Archive(r.Context(), middleware.OrgCtx(r.Context()), did); err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": did, "archived": true})
}

func (h *CommsHandler) NewVersion(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, err := uuid.Parse(chi.URLParam(r, "document_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "multipart body expected")
		return
	}
	f, _, err := r.FormFile("file")
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "file required")
		return
	}
	defer f.Close()
	ver, err := h.svc.NewVersion(r.Context(), oc, did, f)
	if err != nil {
		commsErr(w, err)
		return
	}
	// new blobs go through the same upload pipeline (MIME/executable sniff)
	if t, err := tasks.NewDocProcessTask(did); err == nil {
		_, _ = h.queue.EnqueueContext(r.Context(), t)
	}
	httpx.WriteData(w, 201, map[string]any{"document_id": did, "version": ver})
}

func (h *CommsHandler) DocumentVersions(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	did, err := uuid.Parse(chi.URLParam(r, "document_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	manage := h.org.HasPermission(r.Context(), oc.UserID, oc.OrgID, "org.documents.publish")
	list, err := h.svc.Versions(r.Context(), oc, did, manage)
	if err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

// PublicDocs — GET /directory/orgs/{slug}/docs: visibility=public only.
func (h *CommsHandler) PublicDocs(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.PublicDocs(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		commsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}
