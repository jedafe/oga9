// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// Session resolution: Bearer JWT -> JTI -> live session check (Rule 24 pattern).
package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/auth"
)

type ctxKey string

const (
	CtxUserID     ctxKey = "user_id"
	CtxSessionJTI ctxKey = "session_jti"
)

func RequireAuth(svc *auth.Service, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			if !strings.HasPrefix(h, "Bearer ") {
				writeErr(w, 401, "unauthorized", "missing bearer token")
				return
			}
			claims, err := auth.ParseToken(jwtSecret, strings.TrimPrefix(h, "Bearer "))
			if err != nil {
				writeErr(w, 401, "unauthorized", "invalid token")
				return
			}
			jti, err := uuid.Parse(claims.ID)
			if err != nil {
				writeErr(w, 401, "unauthorized", "bad jti")
				return
			}
			userID, err := svc.ResolveSession(r.Context(), jti)
			if err != nil {
				writeErr(w, 401, "session_revoked", "session expired or revoked")
				return
			}
			ctx := context.WithValue(r.Context(), CtxUserID, userID)
			ctx = context.WithValue(ctx, CtxSessionJTI, jti)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func UserID(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(CtxUserID).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}
func SessionJTI(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(CtxSessionJTI).(uuid.UUID); ok {
		return v
	}
	return uuid.Nil
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "status": status},
	})
}
