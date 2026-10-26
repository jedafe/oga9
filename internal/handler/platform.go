// Ref: oga9-engineering/sections/15-subscription-admin.md — control-plane
// routes (all gated on platform.* nodes via middleware.RequirePlatformNode).
package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/httpx"

	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/platform"
	"github.com/jedafe/oga9/internal/tasks"
)

type PlatformHandler struct {
	svc   *platform.Service
	queue *asynq.Client
}

func NewPlatformHandler(svc *platform.Service, queue *asynq.Client) *PlatformHandler {
	return &PlatformHandler{svc: svc, queue: queue}
}

func platErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, platform.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, platform.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "invalid or missing field")
	case errors.Is(err, platform.ErrProvisionConflict):
		httpx.WriteErr(w, 409, "provision_in_progress", "a provision with this request_id is already running")
	case errors.Is(err, platform.ErrProvisionMismatch):
		httpx.WriteErr(w, 409, "provision_intent_mismatch", "request_id was used with a different request body")
	case errors.Is(err, platform.ErrSessionExpired):
		httpx.WriteErr(w, 404, "support_session_expired", "session expired or already ended")
	default:
		log.Printf("platform: %v", err) // detail stays server-side
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

// CreateTenant — POST /platform/tenants: enqueues tenant:provision
// (critical). The worker claims a durable tenant_provisions row keyed on
// request_id — the row is the idempotency record (created inside the
// pipeline so the enqueue is never blocked on a write).
func (h *PlatformHandler) CreateTenant(w http.ResponseWriter, r *http.Request) {
	var in platform.ProvisionRequest
	if err := httpx.Decode(w, r, &in); err != nil {
		platErr(w, platform.ErrBadValue)
		return
	}
	if in.RequestID == "" || in.TenantSlug == "" || in.OrgSlug == "" || in.OwnerEmail == "" {
		// request_id is the idempotency key — a missing one can never
		// provision; reject up front instead of queuing a doomed task.
		platErr(w, platform.ErrBadValue)
		return
	}
	in.RequestedBy = middleware.UserID(r.Context())
	t, err := tasks.NewProvisionTask(in)
	if err != nil {
		platErr(w, platform.ErrBadValue)
		return
	}
	if _, err := h.queue.EnqueueContext(r.Context(), t); err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 202, map[string]any{"request_id": in.RequestID, "status": "processing"})
}

func (h *PlatformHandler) Tenants(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListTenants(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *PlatformHandler) Tenant(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(chi.URLParam(r, "tenant_id"))
	t, err := h.svc.GetTenant(r.Context(), id)
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, t)
}

func (h *PlatformHandler) SuspendTenant(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(chi.URLParam(r, "tenant_id"))
	var in struct {
		Reason string `json:"reason"`
	}
	_ = httpx.Decode(w, r, &in)
	if err := h.svc.SuspendTenant(r.Context(), id, in.Reason, middleware.UserID(r.Context())); err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"tenant_id": id, "status": "suspended"})
}

func (h *PlatformHandler) ReinstateTenant(w http.ResponseWriter, r *http.Request) {
	id, _ := uuid.Parse(chi.URLParam(r, "tenant_id"))
	if err := h.svc.ReinstateTenant(r.Context(), id, middleware.UserID(r.Context())); err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"tenant_id": id, "status": "active"})
}

func (h *PlatformHandler) Subscriptions(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListSubscriptions(r.Context(), r.URL.Query().Get("state"))
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *PlatformHandler) OverrideSubscription(w http.ResponseWriter, r *http.Request) {
	sid, _ := uuid.Parse(chi.URLParam(r, "subscription_id"))
	var in struct {
		TenantID  uuid.UUID  `json:"tenant_id"`
		Plan      string     `json:"plan"`
		State     string     `json:"state"`
		GraceEnds *time.Time `json:"grace_ends_at"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || in.TenantID == uuid.Nil {
		platErr(w, platform.ErrBadValue)
		return
	}
	if err := h.svc.OverrideSubscription(r.Context(), in.TenantID, sid, in.Plan, in.State, in.GraceEnds, middleware.UserID(r.Context())); err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"subscription_id": sid})
}

// UpsertPlan handles POST /platform/plans and PATCH /platform/plans/{slug}.
func (h *PlatformHandler) UpsertPlan(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug         string `json:"slug"`
		Label        string `json:"label"`
		PriceMonthly int64  `json:"price_monthly_minor"`
		PriceAnnual  int64  `json:"price_annual_minor"`
		Currency     string `json:"currency"`
		IsPublic     bool   `json:"is_public"`
		Sort         int32  `json:"sort"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		platErr(w, platform.ErrBadValue)
		return
	}
	if slug := chi.URLParam(r, "slug"); slug != "" {
		in.Slug = slug
	}
	if in.Slug == "" || in.Label == "" {
		platErr(w, platform.ErrBadValue)
		return
	}
	if in.Currency == "" {
		in.Currency = "USD"
	}
	p, err := h.svc.UpsertPlan(r.Context(), db.UpsertPlanParams{
		Slug: in.Slug, Label: in.Label, PriceMonthlyMinor: pgtype.Int8{Int64: in.PriceMonthly, Valid: in.PriceMonthly != 0},
		PriceAnnualMinor: pgtype.Int8{Int64: in.PriceAnnual, Valid: in.PriceAnnual != 0},
		Currency:         in.Currency, IsPublic: in.IsPublic, Sort: in.Sort}, middleware.UserID(r.Context()))
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, p)
}

func (h *PlatformHandler) Deployments(w http.ResponseWriter, r *http.Request) {
	tid, _ := uuid.Parse(r.URL.Query().Get("tenant_id"))
	list, err := h.svc.ListDeployments(r.Context(), tid)
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *PlatformHandler) CreateDeployment(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TenantID      uuid.UUID `json:"tenant_id"`
		Kind          string    `json:"kind"`
		Edition       string    `json:"edition"`
		DeploymentKey string    `json:"deployment_key"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || in.TenantID == uuid.Nil || in.DeploymentKey == "" {
		platErr(w, platform.ErrBadValue)
		return
	}
	d, err := h.svc.CreateDeployment(r.Context(), in.TenantID, in.Kind, in.Edition, in.DeploymentKey, middleware.UserID(r.Context()))
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 201, d)
}

// Impersonate — POST /platform/support/impersonate (audit-first, 30min TTL).
func (h *PlatformHandler) Impersonate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TenantID    uuid.UUID `json:"tenant_id"`
		TargetEmail string    `json:"target_email"`
		Reason      string    `json:"reason"`
	}
	if err := httpx.Decode(w, r, &in); err != nil || in.TenantID == uuid.Nil || in.TargetEmail == "" {
		platErr(w, platform.ErrBadValue)
		return
	}
	sid, token, exp, err := h.svc.Impersonate(r.Context(), middleware.UserID(r.Context()),
		in.TenantID, in.TargetEmail, in.Reason,
		middleware.ClientIPFrom(r.Context()), r.UserAgent())
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"support_session_id": sid, "token": token, "expires_at": exp})
}

func (h *PlatformHandler) EndImpersonation(w http.ResponseWriter, r *http.Request) {
	sid, _ := uuid.Parse(chi.URLParam(r, "session_id"))
	if err := h.svc.EndImpersonation(r.Context(), sid, middleware.UserID(r.Context())); err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"support_session_id": sid, "ended": true})
}

func (h *PlatformHandler) Audit(w http.ResponseWriter, r *http.Request) {
	tid, _ := uuid.Parse(r.URL.Query().Get("tenant_id"))
	list, err := h.svc.AuditEvents(r.Context(), tid, r.URL.Query().Get("action"))
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *PlatformHandler) Usage(w http.ResponseWriter, r *http.Request) {
	tid, _ := uuid.Parse(r.URL.Query().Get("tenant_id"))
	list, err := h.svc.Usage(r.Context(), tid)
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *PlatformHandler) Flags(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListFlags(r.Context())
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *PlatformHandler) SetFlag(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled bool            `json:"enabled"`
		Rollout json.RawMessage `json:"rollout"`
	}
	if err := httpx.Decode(w, r, &in); err != nil {
		platErr(w, platform.ErrBadValue)
		return
	}
	if err := h.svc.SetFlag(r.Context(), chi.URLParam(r, "key"), in.Enabled, in.Rollout, middleware.UserID(r.Context())); err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"key": chi.URLParam(r, "key"), "enabled": in.Enabled})
}

// MySupportSessions — GET /me/support-sessions (session auth only): the
// §15 banner surface. A tenant sees live support sessions opened on their
// account by operators — no platform node required (it's their own data).
func (h *PlatformHandler) MySupportSessions(w http.ResponseWriter, r *http.Request) {
	uid := middleware.UserID(r.Context())
	rows, err := h.svc.LiveSupportSessions(r.Context(), uid)
	if err != nil {
		platErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"impersonated": len(rows) > 0, "sessions": rows})
}
