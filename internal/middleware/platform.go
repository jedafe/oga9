// Ref: oga9-engineering/conventions/permission-key-naming.md
// platform.* authorization — resolves via NULL-tenant (super-admin) roles
// only; org-cloned roles can never carry platform nodes.
package middleware

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/httpx"
)

// RequirePlatformNode gates on a platform.* permission node (Rule 20 — node
// checks, never role names).
func RequirePlatformNode(pool *pgxpool.Pool, node string) func(http.Handler) http.Handler {
	q := db.New(pool)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ok, err := q.HasPlatformNode(r.Context(), db.HasPlatformNodeParams{
				UserID: pgtype.UUID{Bytes: UserID(r.Context()), Valid: true}, Node: node,
			})
			if err != nil {
				httpx.WriteErr(w, 500, "internal_error", "platform permission check failed")
				return
			}
			if !ok {
				httpx.WriteErr(w, 403, "permission_denied", "missing platform permission")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
