// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// Session resolution: Bearer JWT -> JTI -> live session check (Rule 24 pattern).
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/httpx"
	valkey "github.com/valkey-io/valkey-go"
)

type ctxKey string

const (
	CtxUserID     ctxKey = "user_id"
	CtxSessionJTI ctxKey = "session_jti"
	CtxMFAGate    ctxKey = "mfa_gate"
	CtxTenantID   ctxKey = "tenant_id"
)

func bearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return "", false
	}
	return strings.TrimPrefix(h, "Bearer "), true
}

// resolveSessionToken parses a Bearer JWT and validates the live session row.
// The session row's user must match the token subject — a mismatched
// jti/sub pairing (however unlikely) resolves to nobody.
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
	if claims.Subject != userID.String() {
		return nil, false
	}
	ctx := context.WithValue(r.Context(), CtxUserID, userID)
	return context.WithValue(ctx, CtxSessionJTI, jti), true
}

func RequireAuth(svc *auth.Service, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if UserID(r.Context()) != uuid.Nil || TenantID(r.Context()) != uuid.Nil {
				next.ServeHTTP(w, r) // pre-resolved by APIKeyAuth (user or tenant key)
				return
			}
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
			ctx = context.WithValue(ctx, CtxMFAGate, token) // nonce — consumed on successful confirm
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

// TenantID returns the api-key-resolved tenant identity (tenant-scoped
// keys carry no user — §23). Nil for user sessions.
func TenantID(ctx context.Context) uuid.UUID {
	if v, ok := ctx.Value(CtxTenantID).(uuid.UUID); ok {
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

// MFAGateNonce returns the gate nonce when the request authenticated via an
// mfa_token rather than a full session (empty otherwise).
func MFAGateNonce(ctx context.Context) string {
	v, _ := ctx.Value(CtxMFAGate).(string)
	return v
}

// APIKeyAuth — §23: X-API-Key (or `Bearer oga9_*`) resolves to a
// tenant/user identity through the integrations service. A present-but-
// invalid or revoked key is an immediate 401 (spec test 4); an absent
// header falls through to the session middleware.
type APIKeyResolver interface {
	Authenticate(ctx context.Context, rawKey string) (db.ApiKeys, error)
}

func APIKeyAuth(svc APIKeyResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("X-API-Key")
			if raw == "" {
				if tok := r.Header.Get("Authorization"); strings.HasPrefix(tok, "Bearer oga9_") {
					raw = strings.TrimPrefix(tok, "Bearer ")
				}
			}
			if raw == "" {
				next.ServeHTTP(w, r)
				return
			}
			k, err := svc.Authenticate(r.Context(), raw)
			if err != nil {
				httpx.WriteErr(w, 401, "api_key_revoked", "api key revoked or invalid")
				return
			}
			ctx := context.WithValue(r.Context(), CtxTenantID, uuid.UUID(k.TenantID.Bytes))
			if k.UserID.Valid {
				ctx = context.WithValue(ctx, CtxUserID, uuid.UUID(k.UserID.Bytes))
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireAuthOrWSTicket — §24: browser WebSocket upgrades can't set headers.
// Accepts the session Bearer JWT OR a single-use ?ticket= minted by
// POST /ws/ticket (oga9:ws_ticket:{nonce}, 60s TTL, GETDEL so replay fails).
func RequireAuthOrWSTicket(svc *auth.Service, vk valkey.Client, jwtSecret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if UserID(r.Context()) != uuid.Nil {
				next.ServeHTTP(w, r)
				return
			}
			if token, ok := bearerToken(r); ok {
				if ctx, ok := resolveSessionToken(r, svc, jwtSecret, token); ok {
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
			}
			if t := r.URL.Query().Get("ticket"); t != "" {
				raw, err := vk.Do(r.Context(), vk.B().Getdel().Key("oga9:ws_ticket:"+t).Build()).AsBytes()
				if err == nil {
					if uid, perr := uuid.Parse(strings.Trim(string(raw), `"`)); perr == nil {
						next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), CtxUserID, uid)))
						return
					}
				}
			}
			httpx.WriteErr(w, 401, "unauthorized", "session or ws ticket required")
		})
	}
}
