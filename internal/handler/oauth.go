// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// OAuth2/OIDC flow — providers activate via OGA9_OAUTH_{NAME}_* env config.
package handler

import (
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jedafe/oga9/internal/auth"
	valkey "github.com/valkey-io/valkey-go"
)

type OAuthHandler struct {
	svc       *auth.Service
	vk        valkey.Client
	providers map[string]auth.ProviderConfig
	baseURL   string
}

func NewOAuthHandler(svc *auth.Service, vk valkey.Client, providers map[string]auth.ProviderConfig, baseURL string) *OAuthHandler {
	return &OAuthHandler{svc: svc, vk: vk, providers: providers, baseURL: baseURL}
}

func (h *OAuthHandler) provider(w http.ResponseWriter, r *http.Request) (auth.ProviderConfig, string, bool) {
	name := chi.URLParam(r, "provider")
	p, ok := h.providers[name]
	if !ok {
		writeErr(w, 501, "provider_not_configured", name+" OAuth adapter not yet configured")
		return p, name, false
	}
	return p, name, true
}

// GET /api/v1/auth/oauth/{provider}/start → 302 to provider authorize URL.
func (h *OAuthHandler) Start(w http.ResponseWriter, r *http.Request) {
	p, name, ok := h.provider(w, r)
	if !ok { return }
	url, err := auth.StartOAuth(r.Context(), h.vk, p, h.baseURL+"/api/v1/auth/oauth/"+name+"/callback")
	if err != nil { writeErr(w, 500, "internal_error", "oauth start failed"); return }
	http.Redirect(w, r, url, http.StatusFound)
}

// GET /api/v1/auth/oauth/{provider}/callback?code&state → session.
func (h *OAuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	p, name, ok := h.provider(w, r)
	if !ok { return }
	state, code := r.URL.Query().Get("state"), r.URL.Query().Get("code")
	if state == "" || code == "" {
		writeErr(w, 400, "validation_failed", "state and code required"); return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	token, sessionID, err := h.svc.OAuthCallback(r.Context(), state, code,
		h.baseURL+"/api/v1/auth/oauth/"+name+"/callback", p, name,
		r.UserAgent(), ip, r.UserAgent())
	if err != nil {
		if err == auth.ErrOAuthState { writeErr(w, 400, "oauth_state_invalid", "expired or reused oauth state"); return }
		writeErr(w, 401, "oauth_failed", "provider authentication failed"); return
	}
	writeData(w, 200, map[string]any{"token": token, "session_id": sessionID})
}
