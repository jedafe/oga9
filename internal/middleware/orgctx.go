// Ref: oga9-engineering/sections/04-tenancy-organizations.md (Rule 24)
// Org context resolution: org id from the route is verified against the
// caller's membership server-side — a bare {org_id} param grants nothing.
package middleware

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/tenant"
)

const CtxOrg ctxKey = "org_ctx"

// RequireOrgCtx must run inside a route with {org_id} AFTER RequireAuth —
// resolves user+org to a verified tenant.OrgCtx (oga9:tenant_ctx cache).
func RequireOrgCtx(svc *tenant.Service) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			orgID, err := uuid.Parse(chi.URLParam(r, "org_id"))
			if err != nil {
				httpx.WriteErr(w, 400, "validation_failed", "bad org id")
				return
			}
			oc, err := svc.ResolveOrgCtx(r.Context(), UserID(r.Context()), orgID)
			if err != nil {
				switch err {
				case tenant.ErrOrgNotFound:
					httpx.WriteErr(w, 404, "org_not_found", "organization not found")
				case tenant.ErrNotMember:
					httpx.WriteErr(w, 403, "not_a_member", "not a member of this organization")
				default:
					httpx.WriteErr(w, 500, "internal_error", "org resolution failed")
				}
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), CtxOrg, oc)))
		})
	}
}

// RequireOrgPermission gates on a permission node via tenant role →
// role_permissions (Rule 20 — nodes, never position names). Positions +
// scopes land in Phase 04 (§03); until then tenant-cloned roles carry grants.
func RequireOrgPermission(svc *tenant.Service, node string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			oc := OrgCtx(r.Context())
			if oc == nil || !svc.HasPermission(r.Context(), oc.UserID, oc.TenantID, node) {
				httpx.WriteErr(w, 403, "forbidden", "missing permission "+node)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func OrgCtx(ctx context.Context) *tenant.OrgCtx {
	if v, ok := ctx.Value(CtxOrg).(*tenant.OrgCtx); ok {
		return v
	}
	return nil
}
