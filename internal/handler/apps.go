// Ref: oga9-engineering/sections/06-app-framework-registry.md
// App registry browsing + per-org (tenant) activation routes.
package handler

import (
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tenant"
)

type AppsHandler struct {
	svc *tenant.Service
	ent *entitlements.Service
}

func NewAppsHandler(svc *tenant.Service, ent *entitlements.Service) *AppsHandler {
	return &AppsHandler{svc: svc, ent: ent}
}

func (h *AppsHandler) Registry(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListApps(r.Context())
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "failed to list apps")
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *AppsHandler) OrgApps(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.OrgApps(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *AppsHandler) Activate(w http.ResponseWriter, r *http.Request) {
	err := h.svc.ActivateApp(r.Context(), middleware.OrgCtx(r.Context()), h.ent, chi.URLParam(r, "app_id"))
	switch {
	case err == nil:
		httpx.WriteData(w, 200, map[string]string{"status": "active"})
	case errors.Is(err, tenant.ErrAppNotFound):
		httpx.WriteErr(w, 404, "app_not_found", "unknown app")
	case errors.Is(err, tenant.ErrNotEntitled):
		httpx.WriteErr(w, 402, "not_entitled", "plan does not include this app")
	case errors.Is(err, tenant.ErrAppDepsMissing):
		httpx.WriteErr(w, 409, "app_dependencies_missing", "activate the required apps first")
	default:
		orgErr(w, err)
	}
}

func (h *AppsHandler) Deactivate(w http.ResponseWriter, r *http.Request) {
	err := h.svc.DeactivateApp(r.Context(), middleware.OrgCtx(r.Context()), chi.URLParam(r, "app_id"))
	switch {
	case err == nil:
		httpx.WriteData(w, 200, map[string]string{"status": "deactivated"})
	case errors.Is(err, tenant.ErrAppNotFound):
		httpx.WriteErr(w, 404, "app_not_found", "unknown app")
	case errors.Is(err, tenant.ErrAppRequired):
		httpx.WriteErr(w, 409, "app_required", "MVP-required app cannot be deactivated")
	case errors.Is(err, tenant.ErrAppHasDependents):
		httpx.WriteErr(w, 409, "app_has_dependents", "an active app depends on this one")
	default:
		orgErr(w, err)
	}
}
