// Ref: oga9-engineering/sections/15-subscription-admin.md + ADR-026 —
// tenant-facing export/migration routes: /api/v1/tenants/{tid}/exports.
// Gated on org.admin.export held on tenants.owner_org_id — org admin of
// any other org in the tenant does NOT qualify.
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/platform"
	"github.com/jedafe/oga9/internal/tasks"
)

// PermFunc resolves a permission node for (user, org) — wired to
// tenant.Service.HasPermission so this handler stays off the org module.
type PermFunc func(ctx context.Context, userID, orgID uuid.UUID, node string) bool

type TenantExportHandler struct {
	svc   *platform.Service
	queue *asynq.Client
	perm  PermFunc
}

func NewTenantExportHandler(svc *platform.Service, queue *asynq.Client, perm PermFunc) *TenantExportHandler {
	return &TenantExportHandler{svc: svc, queue: queue, perm: perm}
}

// authorize resolves the tenant's owner org and requires org.admin.export
// on it (Rule 24 — the caller's verified membership decides, not the URL).
func (h *TenantExportHandler) authorize(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return uuid.Nil, false
	}
	ownerOrg, err := h.svc.TenantOwnerOrg(r.Context(), tid)
	if err != nil {
		httpx.WriteErr(w, 404, "not_found", "tenant not found")
		return uuid.Nil, false
	}
	if !h.perm(r.Context(), middleware.UserID(r.Context()), ownerOrg, "org.admin.export") {
		httpx.WriteErr(w, 403, "forbidden", "missing permission org.admin.export")
		return uuid.Nil, false
	}
	return tid, true
}

func tenantExportErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, platform.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "export not found")
	case errors.Is(err, platform.ErrBadExportOption):
		httpx.WriteErr(w, 422, "bad_value", "post_state must be readonly_grace|purge|keep_active")
	case errors.Is(err, platform.ErrExportNotReady):
		httpx.WriteErr(w, 409, "export_not_ready", "export has not finished")
	case errors.Is(err, platform.ErrExportConfirmed):
		httpx.WriteErr(w, 409, "export_confirmed", "export already confirmed")
	case errors.Is(err, platform.ErrPurgeNotConfirmed):
		httpx.WriteErr(w, 422, "confirm_purge_required", "purge requires confirm_purge:true")
	default:
		platErr(w, err)
	}
}

// Create — POST /api/v1/tenants/{tid}/exports: audits, creates the
// durable row, enqueues tenant:export (Rule 8). include_credentials
// requires export_passphrase — it rides the task payload, never the DB.
func (h *TenantExportHandler) Create(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.authorize(w, r)
	if !ok {
		return
	}
	var body struct {
		IncludeCredentials bool   `json:"include_credentials"`
		PostState          string `json:"post_state"`
		ExportPassphrase   string `json:"export_passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpx.WriteErr(w, 400, "invalid_json", "request body is not valid JSON")
		return
	}
	if body.IncludeCredentials && body.ExportPassphrase == "" {
		httpx.WriteErr(w, 422, "passphrase_required", "export_passphrase required when include_credentials is set")
		return
	}
	eid, err := h.svc.RequestTenantExport(r.Context(), tid, middleware.UserID(r.Context()),
		platform.ExportOptions{IncludeCredentials: body.IncludeCredentials, PostState: body.PostState})
	if err != nil {
		tenantExportErr(w, err)
		return
	}
	t, err := tasks.NewTenantExportTask(eid, body.ExportPassphrase)
	if err != nil {
		tenantExportErr(w, err)
		return
	}
	if _, err := h.queue.EnqueueContext(context.WithoutCancel(r.Context()), t); err != nil {
		tenantExportErr(w, err)
		return
	}
	httpx.WriteData(w, 202, map[string]any{"export_id": eid, "status": "processing"})
}

// Get — GET /api/v1/tenants/{tid}/exports/{eid}: status JSON while the
// worker runs; streams the .oga9exp bundle once done.
func (h *TenantExportHandler) Get(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.authorize(w, r)
	if !ok {
		return
	}
	eid, err := uuid.Parse(chi.URLParam(r, "export_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	blob, status, err := h.svc.DownloadTenantExport(r.Context(), tid, eid)
	if err != nil {
		tenantExportErr(w, err)
		return
	}
	if status == "failed" {
		httpx.WriteErr(w, 409, "export_failed", "export failed — request a new one")
		return
	}
	if status != "done" {
		httpx.WriteData(w, 202, map[string]any{"export_id": eid, "status": status})
		return
	}
	defer blob.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="tenant-export-`+eid.String()+`.oga9exp"`)
	w.WriteHeader(200)
	_, _ = io.Copy(w, blob)
}

// Confirm — POST /api/v1/tenants/{tid}/exports/{eid}/confirm: the target
// install verified the import; apply the stored post_state.
func (h *TenantExportHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	tid, ok := h.authorize(w, r)
	if !ok {
		return
	}
	eid, err := uuid.Parse(chi.URLParam(r, "export_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var body struct {
		ConfirmPurge bool `json:"confirm_purge"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpx.WriteErr(w, 400, "invalid_json", "request body is not valid JSON")
		return
	}
	state, err := h.svc.ConfirmTenantMigration(r.Context(), tid, eid,
		middleware.UserID(r.Context()), body.ConfirmPurge)
	if err != nil {
		tenantExportErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"export_id": eid, "post_state": state, "confirmed": true})
}
