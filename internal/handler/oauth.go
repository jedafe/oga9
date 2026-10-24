// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// OAuth plumbing: provider routes registered now; adapters activate when
// provider credentials are configured (google|apple|japaconnect).
package handler

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/jedafe/oga9/internal/auth"
)

type OAuthHandler struct{ svc *auth.Service }
func NewOAuthHandler(svc *auth.Service) *OAuthHandler { return &OAuthHandler{svc: svc} }

// GET /api/v1/auth/oauth/{provider}/start — redirects to provider when
// configured; 501 provider_not_configured until credentials land.
func (h *OAuthHandler) Start(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	switch provider {
	case "google", "apple", "japaconnect":
		writeErr(w, 501, "provider_not_configured",
			provider+" OAuth adapter not yet configured (ADR-021 plumbing)")
	default:
		writeErr(w, 404, "unknown_provider", "unsupported oauth provider")
	}
}

// GET /api/v1/auth/oauth/{provider}/callback — exchange code -> link/provision.
func (h *OAuthHandler) Callback(w http.ResponseWriter, r *http.Request) {
	provider := chi.URLParam(r, "provider")
	switch provider {
	case "google", "apple", "japaconnect":
		writeErr(w, 501, "provider_not_configured", provider+" callback not yet configured")
	default:
		writeErr(w, 404, "unknown_provider", "unsupported oauth provider")
	}
}
