// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// TOTP MFA lifecycle — setup stages a server-side secret (never trust the
// client), confirm validates + issues recovery codes, disable needs password
// + a live code so a hijacked session can't strip MFA.
// setup/confirm also accept an mfa_token (post-credential gate) so accounts
// held at mfa_setup_required can enroll (§02 platform.*/org-admin rule).
package handler

import (
	"errors"
	"net/http"

	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/pquerna/otp/totp"
)

type MFAHandler struct{ svc *auth.Service }

func NewMFAHandler(svc *auth.Service) *MFAHandler { return &MFAHandler{svc: svc} }

func (h *MFAHandler) Setup(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	if on, _ := h.svc.MFAEnabled(r.Context(), userID); on {
		httpx.WriteErr(w, 409, "mfa_already_enabled", "mfa already enabled on this account")
		return
	}
	u, err := h.svc.GetUser(r.Context(), userID)
	if err != nil {
		httpx.WriteErr(w, 404, "user_not_found", "user not found")
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "OGA9", AccountName: u.Email})
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "totp generation failed")
		return
	}
	if err := h.svc.StageMFASecret(r.Context(), userID, key.Secret()); err != nil {
		httpx.WriteErr(w, 500, "internal_error", "mfa staging failed")
		return
	}
	httpx.WriteData(w, 200, map[string]any{
		"secret": key.Secret(), "otpauth_url": key.URL(),
		"note": "confirm with POST /auth/mfa/confirm + code to activate",
	})
}

type mfaConfirmReq struct {
	Code string `json:"code"`
}

func (h *MFAHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	var req mfaConfirmReq
	userID := middleware.UserID(r.Context())
	if err := httpx.Decode(w, r, &req); err != nil || req.Code == "" {
		httpx.WriteErr(w, 400, "validation_failed", "code required")
		return
	}
	secret, err := h.svc.PendingMFASecret(r.Context(), userID)
	if err != nil || secret == "" {
		httpx.WriteErr(w, 400, "mfa_not_staged", "call mfa/setup first")
		return
	}
	if !totp.Validate(req.Code, secret) {
		httpx.WriteErr(w, 400, "invalid_code", "TOTP code invalid")
		return
	}
	codes, err := h.svc.SetupMFA(r.Context(), userID, secret)
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "mfa save failed")
		return
	}
	h.svc.ClearPendingMFA(r.Context(), userID)
	// recovery codes are returned ONCE — store them offline.
	httpx.WriteData(w, 200, map[string]any{"mfa_enabled": true, "recovery_codes": codes})
}

// Complete finishes a gated login: {mfa_token, code} → session. Used by OAuth
// flows (provider redirect can't carry a code) and as an alternative to
// resubmitting credentials after a password login returns mfa_required.
type mfaCompleteReq struct {
	MFAToken string `json:"mfa_token"`
	Code     string `json:"code"`
}

func (h *MFAHandler) Complete(w http.ResponseWriter, r *http.Request) {
	var req mfaCompleteReq
	if err := httpx.Decode(w, r, &req); err != nil || req.MFAToken == "" || req.Code == "" {
		httpx.WriteErr(w, 400, "validation_failed", "mfa_token and code required")
		return
	}
	res, err := h.svc.CompleteMFALogin(r.Context(), req.MFAToken, req.Code)
	if errors.Is(err, auth.ErrMFAGateInvalid) {
		httpx.WriteErr(w, 400, "mfa_gate_invalid", "mfa token expired or already used")
		return
	}
	if errors.Is(err, auth.ErrUserSuspended) {
		httpx.WriteErr(w, 403, "user_suspended", "account suspended")
		return
	}
	if errors.Is(err, auth.ErrMFACode) {
		httpx.WriteErr(w, 401, "invalid_mfa_code", "mfa code incorrect")
		return
	}
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "mfa completion failed")
		return
	}
	httpx.WriteData(w, 200, map[string]any{"token": res.Token, "session_id": res.SessionID})
}

type mfaDisableReq struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (h *MFAHandler) Disable(w http.ResponseWriter, r *http.Request) {
	var req mfaDisableReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Password == "" || req.Code == "" {
		httpx.WriteErr(w, 400, "validation_failed", "password and code required")
		return
	}
	err := h.svc.DisableMFA(r.Context(), middleware.UserID(r.Context()), req.Password, req.Code)
	if errors.Is(err, auth.ErrBadCredentials) {
		httpx.WriteErr(w, 401, "invalid_credentials", "password incorrect")
		return
	}
	if errors.Is(err, auth.ErrMFACode) {
		httpx.WriteErr(w, 401, "invalid_mfa_code", "code incorrect")
		return
	}
	if errors.Is(err, auth.ErrMFANotEnabled) {
		httpx.WriteErr(w, 409, "mfa_not_enabled", "mfa not enabled")
		return
	}
	if err != nil {
		httpx.WriteErr(w, 500, "internal_error", "disable failed")
		return
	}
	httpx.WriteData(w, 200, map[string]any{"mfa_enabled": false})
}
