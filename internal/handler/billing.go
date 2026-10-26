// Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
// Billing surface: subscription lifecycle, plans, entitlements, licenses,
// and the Rule 9 webhook endpoint.
package handler

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"

	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tasks"
	"github.com/jedafe/oga9/internal/tenant"
)

type BillingHandler struct {
	svc    *billing.Service
	orgSvc *tenant.Service
	ent    *entitlements.Service
	queue  *asynq.Client
}

func NewBillingHandler(svc *billing.Service, orgSvc *tenant.Service, ent *entitlements.Service, queue *asynq.Client) *BillingHandler {
	return &BillingHandler{svc: svc, orgSvc: orgSvc, ent: ent, queue: queue}
}

// tenantFor resolves the billing tenant server-side (Rule 24): tenants where
// the caller holds the org.admin.billing node (api-routes-catalog, Rule 20 —
// node, not role name). An explicit ?tenant_id must belong to that set;
// multi-tenant callers must disambiguate.
func (h *BillingHandler) tenantFor(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	userID := middleware.UserID(r.Context())
	admins, err := h.orgSvc.TenantsWithNode(r.Context(), userID, "org.admin.billing")
	if err != nil || len(admins) == 0 {
		httpx.WriteErr(w, 403, "billing_forbidden", "no administered tenant")
		return uuid.Nil, false
	}
	if req := r.URL.Query().Get("tenant_id"); req != "" {
		id, err := uuid.Parse(req)
		if err != nil {
			httpx.WriteErr(w, 400, "invalid_request", "bad tenant_id")
			return uuid.Nil, false
		}
		for _, t := range admins {
			if t == id {
				return id, true
			}
		}
		httpx.WriteErr(w, 403, "billing_forbidden", "missing org.admin.billing on that tenant")
		return uuid.Nil, false
	}
	if len(admins) > 1 {
		httpx.WriteErr(w, 400, "tenant_required", "multiple billing tenants — pass ?tenant_id")
		return uuid.Nil, false
	}
	return admins[0], true
}

// Plans — public plan catalog (platform.plans.view on /platform/plans).
func (h *BillingHandler) Plans(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListPlans(r.Context())
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "plan lookup failed")
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *BillingHandler) Subscription(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	sub, err := h.svc.GetSubscription(r.Context(), tenantID)
	if err != nil {
		if errors.Is(err, billing.ErrNoSubscription) {
			httpx.WriteErr(w, 404, "subscription_not_found", "no subscription for tenant")
			return
		}
		httpx.WriteErr(w, 500, "internal_error", "subscription lookup failed")
		return
	}
	httpx.WriteData(w, 200, sub)
}

type subscriptionReq struct {
	PlanSlug    string `json:"plan_slug"`
	BillingTerm string `json:"billing_term"`
}

func (h *BillingHandler) CreateSubscription(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	var req subscriptionReq
	if err := httpx.Decode(w, r, &req); err != nil || req.PlanSlug == "" {
		httpx.WriteErr(w, 400, "validation_failed", "plan_slug required")
		return
	}
	if req.BillingTerm == "" {
		req.BillingTerm = "monthly"
	}
	if sub, err := h.svc.GetSubscription(r.Context(), tenantID); err == nil && sub.State != "cancelled" {
		httpx.WriteErr(w, 409, "subscription_exists", "use change-plan to switch plans")
		return
	}
	id, err := h.svc.CreateSubscription(r.Context(), tenantID, req.PlanSlug, req.BillingTerm, middleware.UserID(r.Context()).String())
	switch {
	case err == nil:
		httpx.WriteData(w, 201, map[string]uuid.UUID{"subscription_id": id})
	case errors.Is(err, billing.ErrPlanNotFound):
		httpx.WriteErr(w, 404, "plan_not_found", "unknown plan")
	case errors.Is(err, billing.ErrBadBillingTerm):
		httpx.WriteErr(w, 400, "invalid_billing_term", "monthly|annual|enterprise_contract")
	case errors.Is(err, billing.ErrSubscriptionExists):
		httpx.WriteErr(w, 409, "subscription_exists", "use change-plan to switch plans")
	case errors.Is(err, billing.ErrPaymentRequired):
		httpx.WriteErr(w, 402, "payment_required", "paid plans start through provider checkout")
	default:
		httpx.WriteErr(w, 500, "internal_error", "subscription create failed")
	}
}

func (h *BillingHandler) ChangePlan(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	var req subscriptionReq
	if err := httpx.Decode(w, r, &req); err != nil || req.PlanSlug == "" {
		httpx.WriteErr(w, 400, "validation_failed", "plan_slug required")
		return
	}
	err := h.svc.ChangePlan(r.Context(), tenantID, req.PlanSlug, middleware.UserID(r.Context()))
	switch {
	case err == nil:
		httpx.WriteData(w, 200, map[string]string{"status": "plan_changed"})
	case errors.Is(err, billing.ErrNoSubscription):
		httpx.WriteErr(w, 404, "subscription_not_found", "create a subscription first")
	case errors.Is(err, billing.ErrPlanNotFound):
		httpx.WriteErr(w, 404, "plan_not_found", "unknown plan")
	case errors.Is(err, billing.ErrBadTransition):
		httpx.WriteErr(w, 409, "invalid_state_transition", "cancelled subscriptions can't change plan")
	case errors.Is(err, billing.ErrPaymentRequired):
		httpx.WriteErr(w, 402, "payment_required", "upgrades go through provider checkout")
	default:
		httpx.WriteErr(w, 500, "internal_error", "plan change failed")
	}
}

func (h *BillingHandler) Cancel(w http.ResponseWriter, r *http.Request) {
	tenantID, ok := h.tenantFor(w, r)
	if !ok {
		return
	}
	err := h.svc.Cancel(r.Context(), tenantID, middleware.UserID(r.Context()))
	switch {
	case err == nil:
		httpx.WriteData(w, 200, map[string]string{"status": "cancelled"})
	case errors.Is(err, billing.ErrNoSubscription):
		httpx.WriteErr(w, 404, "subscription_not_found", "no subscription for tenant")
	default:
		httpx.WriteErr(w, 500, "internal_error", "cancel failed")
	}
}

// Invoices — empty until platform_invoices lands (group-13 migrations);
// the route shape is catalogued now so clients can integrate.
func (h *BillingHandler) Invoices(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.tenantFor(w, r); !ok {
		return
	}
	httpx.WriteData(w, 200, []any{})
}

// Entitlements — merged capability+limit view per administered tenant.
// Same org.admin.billing node gate as the rest of the /billing surface —
// a delegated billing admin (node, no org_admin role) sees this too.
func (h *BillingHandler) Entitlements(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	admins, err := h.orgSvc.TenantsWithNode(r.Context(), userID, "org.admin.billing")
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "tenant lookup failed")
		return
	}
	out := map[string]any{}
	for _, t := range admins {
		set, err := h.ent.Set(r.Context(), t)
		if err != nil {
			log.Printf("entitlements: resolve failed for tenant %s: %v", t, err)
			continue
		}
		out[t.String()] = set
	}
	httpx.WriteData(w, 200, out)
}

// ---------- licenses (self-hosted, Rule 29) ----------

type licenseReq struct {
	TenantID     string          `json:"tenant_id"`
	DeploymentID string          `json:"deployment_id"`
	Edition      string          `json:"edition"`
	Entitlements json.RawMessage `json:"entitlements"`
	ExpiresAt    string          `json:"expires_at"` // RFC3339
}

func (h *BillingHandler) IssueLicense(w http.ResponseWriter, r *http.Request) {
	var req licenseReq
	if err := httpx.Decode(w, r, &req); err != nil || req.DeploymentID == "" {
		httpx.WriteErr(w, 400, "validation_failed", "deployment_id + entitlements + expires_at required")
		return
	}
	tenantID, err := uuid.Parse(req.TenantID)
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_request", "bad tenant_id")
		return
	}
	exp, err := time.Parse(time.RFC3339, req.ExpiresAt)
	if err != nil || len(req.Entitlements) == 0 {
		httpx.WriteErr(w, 400, "invalid_request", "expires_at must be RFC3339; entitlements required")
		return
	}
	if req.Edition == "" {
		req.Edition = "self_hosted"
	}
	id, err := h.svc.IssueLicense(r.Context(), tenantID, req.DeploymentID, req.Edition, req.Entitlements, exp, middleware.UserID(r.Context()))
	switch {
	case err == nil:
		httpx.WriteData(w, 201, map[string]uuid.UUID{"license_id": id})
	case errors.Is(err, billing.ErrTenantNotFound):
		httpx.WriteErr(w, 404, "tenant_not_found", "unknown tenant")
	case errors.Is(err, billing.ErrBadExpiry):
		httpx.WriteErr(w, 400, "invalid_expiry", "expires_at must be in the future")
	default:
		httpx.WriteErr(w, 500, "internal_error", "license issue failed")
	}
}

type validateReq struct {
	DeploymentID string `json:"deployment_id"`
	Signature    string `json:"signature"` // hex ed25519 from the issued license file
}

// ValidateLicense is the self-hosted callback — no auth; the signed
// license material is the credential (Rule 29 offline model).
func (h *BillingHandler) ValidateLicense(w http.ResponseWriter, r *http.Request) {
	var req validateReq
	if err := httpx.Decode(w, r, &req); err != nil || req.DeploymentID == "" || req.Signature == "" {
		httpx.WriteErr(w, 400, "validation_failed", "deployment_id + signature required")
		return
	}
	ent, err := h.svc.ValidateLicense(r.Context(), req.DeploymentID, req.Signature)
	switch {
	case err == nil:
		httpx.WriteData(w, 200, map[string]any{"valid": true, "entitlements": json.RawMessage(ent)})
	case errors.Is(err, billing.ErrLicenseNotFound):
		httpx.WriteErr(w, 404, "license_not_found", "unknown deployment")
	case errors.Is(err, billing.ErrLicenseExpired):
		httpx.WriteErr(w, 410, "license_expired", "license expired — renewal required")
	case errors.Is(err, billing.ErrBadSignature):
		httpx.WriteErr(w, 401, "invalid_signature", "signature verification failed")
	default:
		httpx.WriteErr(w, 500, "internal_error", "license validation failed")
	}
}

// LicenseFile — GET /platform/licenses/{deployment_id}/file: the signed
// document a deployment mounts at OGA9_LICENSE_FILE (§14/§15 catalog).
func (h *BillingHandler) LicenseFile(w http.ResponseWriter, r *http.Request) {
	b, err := h.svc.LicenseFile(r.Context(), chi.URLParam(r, "deployment_id"))
	switch {
	case err == nil:
		var doc json.RawMessage = b
		httpx.WriteData(w, 200, doc)
	case errors.Is(err, billing.ErrLicenseNotFound):
		httpx.WriteErr(w, 404, "license_not_found", "unknown deployment")
	default:
		httpx.WriteErr(w, 500, "internal_error", "license file failed")
	}
}

// Webhook — Rule 9: verify signature → durable record → enqueue → return
// fast. Provider signature is HMAC-SHA256 (hex) over the raw body in the
// X-Webhook-Signature header.
func (h *BillingHandler) Webhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		httpx.WriteErr(w, 400, "invalid_request", "empty webhook body")
		return
	}
	sig, err := hex.DecodeString(r.Header.Get("X-Webhook-Signature"))
	if err != nil {
		httpx.WriteErr(w, 401, "invalid_signature", "missing/invalid signature header")
		return
	}
	id, err := h.svc.RecordWebhook(r.Context(), chi.URLParam(r, "provider"), body, sig)
	switch {
	case errors.Is(err, billing.ErrDuplicateEvent):
		httpx.WriteData(w, 200, map[string]bool{"duplicate": true})
		return
	case errors.Is(err, billing.ErrWebhookUnconfigured):
		httpx.WriteErr(w, 503, "webhook_provider_not_configured", "no secret configured")
		return
	case errors.Is(err, billing.ErrBadSignature):
		httpx.WriteErr(w, 401, "invalid_signature", "signature verification failed")
		return
	case errors.Is(err, billing.ErrBadPayload):
		httpx.WriteErr(w, 400, "invalid_webhook_payload", "signed body missing id/type")
		return
	case err != nil:
		httpx.WriteErr(w, 500, "internal_error", "webhook record failed")
		return
	}
	t, terr := tasks.NewProcessWebhookTask(id.String())
	if terr == nil {
		if _, err := h.queue.EnqueueContext(context.WithoutCancel(r.Context()), t); err != nil {
			log.Printf("webhook enqueue failed (%s): %v", id, err)
		}
	}
	httpx.WriteData(w, 202, map[string]uuid.UUID{"raw_event_id": id})
}
