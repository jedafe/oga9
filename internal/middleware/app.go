// Ref: oga9-engineering/sections/06-app-framework-registry.md
// App-activation gate — §06 "deactivation disables functionality, never
// deletes". Module routes declare their owning app; the gate resolves the
// org's tenant server-side (OrgCtx, Rule 24) and rejects when the app is
// not installed+active. Consumed by module route groups as they land
// (Phase 08+: e.g. r.With(RequireAppActive(svc, "finance_lite"))).
package middleware

import (
	"net/http"

	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/tenant"
)

// RequireAppActive must run AFTER RequireOrgCtx — it reads the resolved
// tenant from context, never from a request parameter.
func RequireAppActive(svc *tenant.Service, appID string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			oc := OrgCtx(r.Context())
			if oc == nil {
				httpx.WriteErr(w, 500, "internal_error", "app gate requires org context")
				return
			}
			active, err := svc.IsAppActive(r.Context(), oc.TenantID, appID)
			if err != nil {
				httpx.WriteErr(w, 500, "internal_error", "app status check failed")
				return
			}
			if !active {
				httpx.WriteErr(w, 403, "app_not_active", "app is not active for this organization")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
