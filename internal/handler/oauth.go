// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// OAuth2/OIDC flow — providers activate via OGA9_OAUTH_{NAME}_* env config.
package handler

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/hibiken/asynq"
	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tasks"
	valkey "github.com/valkey-io/valkey-go"
)

type OAuthHandler struct {
	svc       *auth.Service
	vk        valkey.Client
	providers map[string]auth.ProviderConfig
	baseURL   string
	appURL    string
	queue     *asynq.Client
	secret    string
}

func NewOAuthHandler(svc *auth.Service, vk valkey.Client, providers map[string]auth.ProviderConfig, baseURL, appURL string, queue *asynq.Client, secret string) *OAuthHandler {
	return &OAuthHandler{svc: svc, vk: vk, providers: providers, baseURL: baseURL, appURL: appURL, queue: queue, secret: secret}
}

func (h *OAuthHandler) provider(w http.ResponseWriter, r *http.Request) (auth.ProviderConfig, string, bool) {
	name := chi.URLParam(r, "provider")
	p, ok := h.providers[name]
	if !ok {
		httpx.WriteErr(w, 501, "provider_not_configured", name+" OAuth adapter not yet configured")
		return p, name, false
	}
	return p, name, true
}

// GET /api/v1/auth/oauth/{provider}/start → 302 to provider authorize URL.
func (h *OAuthHandler) Start(w http.ResponseWriter, r *http.Request) {
	p, name, ok := h.provider(w, r)
	if !ok {
		return
	}
	url, err := auth.StartOAuth(r.Context(), h.vk, p, h.baseURL+"/api/v1/auth/oauth/"+name+"/callback")
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "oauth start failed")
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
}

// GET /api/v1/auth/oauth/{provider}/callback?code&state → session, or an
// mfa_required response carrying mfa_token for /auth/mfa/complete.
func (h *OAuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	p, name, ok := h.provider(w, r)
	if !ok {
		return
	}
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" {
		httpx.WriteErr(w, 400, "validation_failed", "state and code required")
		return
	}
	res, err := h.svc.OAuthCallback(r.Context(), state, code,
		h.baseURL+"/api/v1/auth/oauth/"+name+"/callback", p, name,
		r.URL.Query().Get("device_label"), middleware.ClientIPFrom(r.Context()), r.UserAgent())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrOAuthState):
			httpx.WriteErr(w, 400, "oauth_state_invalid", "expired or reused oauth state")
		case errors.Is(err, auth.ErrMFARequired) && res != nil:
			httpx.WriteErrDetails(w, 403, "mfa_required", "mfa code required",
				map[string]any{"mfa_token": res.MFAToken})
		case errors.Is(err, auth.ErrMFASetupRequired) && res != nil:
			httpx.WriteErrDetails(w, 403, "mfa_setup_required",
				"this account must enroll mfa before login; use mfa_token with /auth/mfa/setup then /auth/mfa/confirm",
				map[string]any{"mfa_token": res.MFAToken})
		case errors.Is(err, auth.ErrUserSuspended):
			httpx.WriteErr(w, 403, "user_suspended", "account suspended")
		case errors.Is(err, auth.ErrEmailNotVerified):
			// Provider didn't verify the address — send the standard
			// verification link so the account isn't stranded.
			if res != nil && res.Email != "" {
				if uid, _ := h.svc.UserIDByEmail(r.Context(), res.Email); uid != "" {
					token, _ := auth.SignActionToken(h.secret, uid, "verify_email", 24*time.Hour)
					t, terr := tasks.NewEmailTask(tasks.TypeSendVerification,
						tasks.SendEmailPayload{To: res.Email, Subject: "Verify your OGA9 account",
							Body: "Verify your account: " + h.appURL + "/verify/" + token})
					if terr == nil {
						if _, err := h.queue.EnqueueContext(context.WithoutCancel(r.Context()), t); err != nil {
							log.Printf("mail enqueue failed (%s → %s): %v", tasks.TypeSendVerification, res.Email, err)
						}
					}
				}
			}
			httpx.WriteErr(w, 403, "email_not_verified", "verify your email — a verification link was sent")
		case errors.Is(err, auth.ErrOAuthNoEmail):
			httpx.WriteErr(w, 400, "oauth_email_missing", "provider returned no email address")
		default:
			httpx.WriteErr(w, 401, "oauth_failed", "provider authentication failed")
		}
		return
	}
	// A real app deployment redirects the browser back to the frontend with
	// the session; pure-API installs (AppURL unset/same as base) get JSON.
	if h.appURL != "" && h.appURL != h.baseURL {
		q := url.Values{"token": {res.Token}, "session_id": {res.SessionID.String()}}
		http.Redirect(w, r, h.appURL+"/auth/callback?"+q.Encode(), http.StatusFound)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"token": res.Token, "session_id": res.SessionID})
}
