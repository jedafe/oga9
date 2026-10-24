// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// Session resolution: Bearer JWT -> JTI -> live session check (Rule 24 pattern).
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/httpx"
)

type ctxKey string

const (
	CtxUserID     ctxKey = "user_id"
	CtxSessionJTI ctxKey = "session_jti"
	CtxMFAGate    ctxKey = "mfa_gate"
)

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	return strings.TrimPrefix(h, "Bearer "), true
}

// resolveSessionToken parses a Bearer JWT and validates the live session row.
func resolveSessionToken(r *http.Request, svc *auth.Service, jwtSecret, token string) (context.Context, bool) {
	claims, err := auth.ParseToken(jwtSecret, token)
	if err != nil {
		return nil, false
	}
	jti, err := uuid.Parse(claims.ID)
	if err != nil {
		return nil, false
	}
	userID, err := svc.ResolveSession(r.Context(), jti)
	if err != nil {
		return nil, false
	}
	ctx := context.WithValue(r.Context(), CtxUserID, userID)
	return context.WithValue(ctx, CtxSessionJTI, jti), true
}

func RequireAuth(svc *auth.Service, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				httpx.WriteErr(w, 401, "unauthorized", "missing bearer token")
				return
			}
			ctx, ok := resolveSessionToken(r, svc, jwtSecret, token)
			if !ok {
				httpx.WriteErr(w, 401, "session_revoked", "session expired or revoked")
				return
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAuthOrMFAGate accepts a full session OR an mfa gate nonce as the
// Bearer credential — used on mfa/setup + mfa/confirm so a privileged login
// held at mfa_setup_required can still enroll (§02).
func RequireAuthOrMFAGate(svc *auth.Service, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token, ok := bearerToken(r)
			if !ok {
				httpx.WriteErr(w, 401, "unauthorized", "missing bearer token")
				return
			}
			if ctx, ok := resolveSessionToken(r, svc, jwtSecret, token); ok {
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			userID, err := svc.ResolveMFAGate(r.Context(), token)
			if err != nil {
				httpx.WriteErr(w, 401, "session_revoked", "session expired, revoked, or invalid mfa token")
				return
			}
			ctx := context.WithValue(r.Context(), CtxUserID, userID)
			ctx = context.WithValue(ctx, CtxMFAGate, true)
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
func ViaMFAGate(ctx context.Context) bool {
	v, _ := ctx.Value(CtxMFAGate).(bool)
	return v
}
