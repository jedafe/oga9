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

// RequireAnyOrgPermission gates a route when ANY of the listed nodes
// passes — for surfaces where the spec names alternatives (e.g. voice
// room end: "room starter or org.chat.moderate" — the service still
// checks the starter path itself; a moderator needs the node).
func RequireAnyOrgPermission(svc *tenant.Service, nodes ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			oc := OrgCtx(r.Context())
			if oc == nil {
				httpx.WriteErr(w, 500, "internal_error", "permission gate requires org context")
				return
			}
			for _, n := range nodes {
				if svc.HasPermission(r.Context(), oc.UserID, oc.OrgID, n) {
					next.ServeHTTP(w, r)
					return
				}
			}
			httpx.WriteErr(w, 403, "forbidden", "missing required permission")
		})
	}
}
