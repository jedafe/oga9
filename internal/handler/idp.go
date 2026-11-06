// Ref: oga9-engineering/sections/29-wordpress-connector.md + ADR-028 —
// OIDC/OAuth2 provider endpoints + platform client management.
package handler

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

type IDPHandler struct {
	svc    *auth.Service
	appURL string
}

func NewIDPHandler(svc *auth.Service, appURL string) *IDPHandler {
	return &IDPHandler{svc: svc, appURL: appURL}
}

func idpErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrClientNotFound):
		httpx.WriteErr(w, 404, "client_not_found", "unknown client_id")
	case errors.Is(err, auth.ErrClientDisabled):
		httpx.WriteErr(w, 403, "client_disabled", "client is disabled")
	case errors.Is(err, auth.ErrBadClientState):
		httpx.WriteErr(w, 400, "invalid_request", "status must be active or disabled")
	case errors.Is(err, auth.ErrBadRedirectURI):
		httpx.WriteErr(w, 400, "invalid_request", "redirect_uri not registered for this client")
	case errors.Is(err, auth.ErrBadClientSecret):
		httpx.WriteErr(w, 401, "invalid_client", "client authentication failed")
	case errors.Is(err, auth.ErrCodeInvalid), errors.Is(err, auth.ErrGrantInvalid):
		httpx.WriteErr(w, 400, "invalid_grant", "grant invalid, expired, or consumed")
	case errors.Is(err, auth.ErrBadPKCE):
		httpx.WriteErr(w, 400, "invalid_grant", "PKCE verification failed")
	case errors.Is(err, auth.ErrConsentRequired):
		httpx.WriteErr(w, 403, "consent_required", "user consent required — POST /oauth/consent then retry")
	case errors.Is(err, auth.ErrAccessDenied):
		httpx.WriteErr(w, 403, "access_denied", "user is not a member of the client's organization")
	default:
		httpx.WriteErr(w, 500, "internal_error", "oauth request failed")
	}
}

// Authorize — GET /oauth/authorize (browser redirect flow). Requires a
// live §02 session; unauthenticated → 302 to the app login with
// return_to. On success → 302 redirect_uri?code=&state=.
func (h *IDPHandler) Authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	clientID := q.Get("client_id")
	redirectURI := q.Get("redirect_uri")
	state := q.Get("state")
	if q.Get("response_type") != "code" || clientID == "" || redirectURI == "" {
		httpx.WriteErr(w, 400, "invalid_request", "response_type=code, client_id and redirect_uri required")
		return
	}
	uid := middleware.UserID(r.Context())
	if uid == uuid.Nil {
		target := h.appURL + "/login?return_to=" + url.QueryEscape(r.URL.RequestURI())
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	code, err := h.svc.Authorize(r.Context(), uid, clientID, redirectURI,
		q.Get("code_challenge"), q.Get("code_challenge_method"),
		q.Get("consent") == "accept")
	if err != nil {
		if errors.Is(err, auth.ErrAccessDenied) && redirectURI != "" {
			sep := "?"
			u, _ := url.Parse(redirectURI)
			if u != nil && u.Query().Encode() != "" {
				sep = "&"
			}
			loc := redirectURI + sep + "error=access_denied"
			if state != "" {
				loc += "&state=" + url.QueryEscape(state)
			}
			http.Redirect(w, r, loc, http.StatusFound) // #nosec G710 -- redirect_uri validated against registered URIs
			return
		}
		idpErr(w, err)
		return
	}
	u, err := url.Parse(redirectURI)
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_request", "bad redirect_uri")
		return
	}
	qq := u.Query()
	qq.Set("code", code)
	if state != "" {
		qq.Set("state", state)
	}
	u.RawQuery = qq.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound) // #nosec G710 -- redirect_uri validated against registered URIs
}

type tokenReq struct {
	GrantType    string `json:"grant_type"`
	Code         string `json:"code"`
	RedirectURI  string `json:"redirect_uri"`
	CodeVerifier string `json:"code_verifier"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

// Token — POST /oauth/token. Accepts form-encoded (RFC 6749) or JSON.
func (h *IDPHandler) Token(w http.ResponseWriter, r *http.Request) {
	var req tokenReq
	if err := r.ParseForm(); err == nil && r.Form.Get("grant_type") != "" {
		req = tokenReq{
			GrantType: r.Form.Get("grant_type"), Code: r.Form.Get("code"),
			RedirectURI: r.Form.Get("redirect_uri"), CodeVerifier: r.Form.Get("code_verifier"),
			ClientID: r.Form.Get("client_id"), ClientSecret: r.Form.Get("client_secret"),
			RefreshToken: r.Form.Get("refresh_token"),
		}
	} else if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "invalid_request", "request body is not valid")
		return
	}
	ip, ua := middleware.ClientIPFrom(r.Context()), r.UserAgent()
	var res *auth.TokenResult
	var err error
	switch req.GrantType {
	case "authorization_code":
		res, err = h.svc.ExchangeCode(r.Context(), req.ClientID, req.ClientSecret,
			req.Code, req.RedirectURI, req.CodeVerifier, ip, ua)
	case "refresh_token":
		res, err = h.svc.RefreshGrant(r.Context(), req.ClientID, req.ClientSecret,
			req.RefreshToken, ip, ua)
	default:
		httpx.WriteErr(w, 400, "unsupported_grant_type", "grant_type must be authorization_code or refresh_token")
		return
	}
	if err != nil {
		idpErr(w, err)
		return
	}
	httpx.WriteData(w, 200, res)
}

// Userinfo — GET /oauth/userinfo (bearer = oauth session access token).
func (h *IDPHandler) Userinfo(w http.ResponseWriter, r *http.Request) {
	claims, err := h.svc.UserinfoClaims(r.Context(), middleware.SessionJTI(r.Context()))
	if err != nil {
		idpErr(w, err)
		return
	}
	httpx.WriteData(w, 200, claims)
}

// Revoke — POST /oauth/revoke: with a `token` body field, revokes the
// caller's grant by refresh token (RFC 7009); otherwise revokes the
// grant bound to the caller's session.
func (h *IDPHandler) Revoke(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	_ = httpx.Decode(w, r, &body)
	if body.Token != "" {
		if err := h.svc.RevokeTokenGrant(r.Context(), middleware.UserID(r.Context()), body.Token); err != nil {
			idpErr(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := h.svc.RevokeSessionGrant(r.Context(), middleware.SessionJTI(r.Context())); err != nil {
		idpErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// RotateClientSecret — POST /platform/oauth-clients/{cid}/rotate-secret:
// kills the old secret, returns the new one once.
func (h *IDPHandler) RotateClientSecret(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "cid"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "bad client id")
		return
	}
	secret, err := h.svc.RotateOAuthClientSecret(r.Context(), id)
	if err != nil {
		idpErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"client_secret": secret})
}

// ---------- platform client management (platform.clients.manage) ----------

// ListClients — GET /platform/oauth-clients.
func (h *IDPHandler) ListClients(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListOAuthClients(r.Context())
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "list failed")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, map[string]any{
			"id": uuid.UUID(c.ID.Bytes), "kind": c.Kind, "client_id": c.ClientID,
			"name": c.Name, "redirect_uris": c.RedirectUris, "trusted": c.Trusted,
			"status": c.Status, "created_at": c.CreatedAt.Time,
		})
	}
	httpx.WriteData(w, 200, out)
}

// CreateClient — POST /platform/oauth-clients. The secret is returned
// once and never recoverable.
func (h *IDPHandler) CreateClient(w http.ResponseWriter, r *http.Request) {
	var in auth.OAuthClientIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "invalid_json", "request body is not valid JSON")
		return
	}
	c, secret, err := h.svc.CreatePlatformClient(r.Context(), in)
	if err != nil {
		idpErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{
		"id": uuid.UUID(c.ID.Bytes), "client_id": c.ClientID, "client_secret": secret,
		"name": c.Name, "redirect_uris": c.RedirectUris, "trusted": c.Trusted,
	})
}

// UpdateClient — PATCH /platform/oauth-clients/{cid}.
func (h *IDPHandler) UpdateClient(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "cid"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "bad client id")
		return
	}
	var in auth.OAuthClientIn
	if err := httpx.Decode(w, r, &in); err != nil {
		httpx.WriteErr(w, 400, "invalid_json", "request body is not valid JSON")
		return
	}
	if err := h.svc.UpdateOAuthClient(r.Context(), id, in); err != nil {
		idpErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"updated": true})
}

// DeleteClient — DELETE /platform/oauth-clients/{cid}: disables the
// client and revokes all its grants.
func (h *IDPHandler) DeleteClient(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "cid"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "bad client id")
		return
	}
	if err := h.svc.SetOAuthClientStatus(r.Context(), id, "disabled"); err != nil {
		idpErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Consent — POST /oauth/consent {client_id}: persists the user's consent
// decision for an untrusted client (ADR-028).
func (h *IDPHandler) Consent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ClientID string `json:"client_id"`
	}
	if err := httpx.Decode(w, r, &body); err != nil || body.ClientID == "" {
		httpx.WriteErr(w, 400, "invalid_request", "client_id required")
		return
	}
	if err := h.svc.GrantConsent(r.Context(), middleware.UserID(r.Context()), body.ClientID); err != nil {
		idpErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Discovery — GET /.well-known/openid-configuration.
func (h *IDPHandler) Discovery(w http.ResponseWriter, r *http.Request) {
	httpx.WriteData(w, 200, h.svc.DiscoveryDoc())
}

// JWKS — GET /oauth/jwks.json.
func (h *IDPHandler) JWKS(w http.ResponseWriter, r *http.Request) {
	httpx.WriteData(w, 200, h.svc.JWKSet())
}

// CreateConnectorClient — POST /orgs/{org_id}/oauth-clients: mints an
// org-scoped connector client bound to a connector row (§29).
func (h *IDPHandler) CreateConnectorClient(w http.ResponseWriter, r *http.Request) {
	oc := middleware.OrgCtx(r.Context())
	var body struct {
		ConnectorID  string   `json:"connector_id"`
		Name         string   `json:"name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := httpx.Decode(w, r, &body); err != nil || body.ConnectorID == "" {
		httpx.WriteErr(w, 400, "invalid_request", "connector_id required")
		return
	}
	cid, err := uuid.Parse(body.ConnectorID)
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "bad connector id")
		return
	}
	c, secret, err := h.svc.CreateConnectorClient(r.Context(), oc.OrgID, cid, body.Name, body.RedirectURIs)
	if err != nil {
		idpErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{
		"id": uuid.UUID(c.ID.Bytes), "client_id": c.ClientID, "client_secret": secret,
		"name": c.Name, "redirect_uris": c.RedirectUris,
	})
}
