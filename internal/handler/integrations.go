// Ref: oga9-engineering/sections/23-integrations.md — §23 integration routes.
package handler

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/integrations"
	"github.com/jedafe/oga9/internal/middleware"
)

type IntegrationsHandler struct {
	svc        *integrations.Service
	queue      *asynq.Client
	baseDomain string
}

func NewIntegrationsHandler(s *integrations.Service, queue *asynq.Client, baseDomain string) *IntegrationsHandler {
	return &IntegrationsHandler{svc: s, queue: queue, baseDomain: baseDomain}
}

func intErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, integrations.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, integrations.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, integrations.ErrBadSignature):
		httpx.WriteErr(w, 400, "invalid_signature", "webhook signature verification failed")
	case errors.Is(err, integrations.ErrRevoked):
		httpx.WriteErr(w, 401, "api_key_revoked", "api key revoked or invalid")
	default:
		log.Printf("integrations: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

// ---------- connectors ----------

type connectorReq struct {
	Kind          string          `json:"kind"`
	Config        json.RawMessage `json:"config"`
	CredentialRef string          `json:"credential_ref"`
	Enabled       *bool           `json:"enabled"`
}

func (h *IntegrationsHandler) ListConnectors(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListConnectors(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *IntegrationsHandler) Configure(w http.ResponseWriter, r *http.Request) {
	var req connectorReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Kind == "" {
		httpx.WriteErr(w, 400, "validation_failed", "kind required")
		return
	}
	if len(req.Config) == 0 {
		req.Config = json.RawMessage("{}")
	}
	id, err := h.svc.Configure(r.Context(), middleware.OrgCtx(r.Context()),
		req.Kind, req.Config, req.CredentialRef)
	if err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"connector_id": id})
}

func (h *IntegrationsHandler) SetEnabled(w http.ResponseWriter, r *http.Request) {
	cid, err := uuid.Parse(chi.URLParam(r, "connector_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req connectorReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Enabled == nil {
		httpx.WriteErr(w, 400, "validation_failed", "enabled required")
		return
	}
	if err := h.svc.SetEnabled(r.Context(), middleware.OrgCtx(r.Context()), cid, *req.Enabled); err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"enabled": *req.Enabled})
}

// ---------- webhooks ----------

// Webhook — POST /webhooks/{provider}: verify → persist → 200 fast.
// Processing is async (integrations:webhook_process sweep).
func (h *IntegrationsHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(raw) == 0 {
		httpx.WriteErr(w, 400, "bad_payload", "empty body")
		return
	}
	sig := r.Header.Get("X-Paystack-Signature")
	if sig == "" {
		sig = r.Header.Get("Verif-Hash") // flutterwave
	}
	if sig == "" {
		sig = r.Header.Get("X-Signature")
	}
	rid, ok, err := h.svc.Intake(r.Context(), provider, raw, sig)
	if err != nil && !errors.Is(err, integrations.ErrBadSignature) {
		intErr(w, err)
		return
	}
	if !ok {
		// receipt persisted — reject the delivery (spec test 2).
		httpx.WriteErr(w, 400, "invalid_signature", "webhook signature verification failed")
		return
	}
	// Rule 9: record raw → enqueue durable processing → return fast.
	// Fire-and-forget (Rule 8); the 1-minute sweep covers enqueue misses.
	if h.queue != nil {
		payload, _ := json.Marshal(map[string]any{"receipt_id": rid.String()})
		_, _ = h.queue.Enqueue(asynq.NewTask("integrations:webhook_process", payload),
			asynq.Queue("critical"))
	}
	httpx.WriteData(w, 200, map[string]any{"received": true})
}

// ---------- locator + directory ----------

type joinReq struct {
	Email         string          `json:"email"`
	Phone         string          `json:"phone"`
	JapaSubject   string          `json:"japa_subject"`
	ReferralToken string          `json:"referral_token"` // signed japa referral (§23)
	Answers       json.RawMessage `json:"answers"`
}

func (h *IntegrationsHandler) LocatorJoin(w http.ResponseWriter, r *http.Request) {
	var req joinReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Email == "" {
		httpx.WriteErr(w, 400, "validation_failed", "email required")
		return
	}
	id, err := h.svc.LocatorJoin(r.Context(), chi.URLParam(r, "slug"),
		req.Email, req.Phone, req.JapaSubject, req.ReferralToken, req.Answers)
	if err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"application_id": id})
}

// ---------- ICS ----------

func (h *IntegrationsHandler) Calendar(w http.ResponseWriter, r *http.Request) {
	ics, err := h.svc.Calendar(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		intErr(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/calendar; charset=utf-8")
	w.WriteHeader(200)
	_, _ = w.Write([]byte(ics))
}

// ---------- /me/api-keys ----------

type keyReq struct {
	Scopes []string `json:"scopes"`
}

func (h *IntegrationsHandler) ListKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListKeys(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *IntegrationsHandler) IssueKey(w http.ResponseWriter, r *http.Request) {
	var req keyReq
	_ = httpx.Decode(w, r, &req)
	id, raw, err := h.svc.IssueKey(r.Context(), middleware.UserID(r.Context()), req.Scopes)
	if err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"key_id": id, "api_key": raw})
}

func (h *IntegrationsHandler) RevokeKey(w http.ResponseWriter, r *http.Request) {
	kid, err := uuid.Parse(chi.URLParam(r, "key_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.Revoke(r.Context(), middleware.UserID(r.Context()), kid); err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"revoked": true})
}

// OrgProfile — GET /directory/orgs/{slug}: the portal landing payload —
// opted-in profile fields + branding + current-term EXCO roster.
func (h *IntegrationsHandler) OrgProfile(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.PublicOrgProfile(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		httpx.WriteErr(w, 404, "not_found", "org not listed")
		return
	}
	httpx.WriteData(w, 200, out)
}

// ResolveHost — GET /directory/resolve?host={h}: host→org map for the
// portal; `{slug}.{base}` subdomains and org custom_domains both resolve.
func (h *IntegrationsHandler) ResolveHost(w http.ResponseWriter, r *http.Request) {
	host := r.URL.Query().Get("host")
	if host == "" {
		httpx.WriteErr(w, 400, "validation_failed", "host required")
		return
	}
	slug, kind, err := h.svc.ResolveHost(r.Context(), host, h.baseDomain)
	if err != nil {
		httpx.WriteErr(w, 404, "not_found", "no organization at that address")
		return
	}
	httpx.WriteData(w, 200, map[string]any{"slug": slug, "host_kind": kind})
}

// ---------- tenant-scoped api keys (§23 — control-plane surface) ----------

func (h *IntegrationsHandler) ListTenantKeys(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	list, err := h.svc.ListTenantKeys(r.Context(), tid)
	if err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *IntegrationsHandler) IssueTenantKey(w http.ResponseWriter, r *http.Request) {
	tid, err := uuid.Parse(chi.URLParam(r, "tenant_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Scopes []string `json:"scopes"`
	}
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "malformed body")
		return
	}
	id, raw, err := h.svc.IssueTenantKey(r.Context(), tid, req.Scopes)
	if err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"key_id": id, "key": raw}) // raw returned ONCE
}

func (h *IntegrationsHandler) RevokeTenantKey(w http.ResponseWriter, r *http.Request) {
	tid, err1 := uuid.Parse(chi.URLParam(r, "tenant_id"))
	kid, err2 := uuid.Parse(chi.URLParam(r, "key_id"))
	if err1 != nil || err2 != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.RevokeTenantKey(r.Context(), tid, kid); err != nil {
		intErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"revoked": true})
}
