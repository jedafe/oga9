// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// TOTP MFA lifecycle — setup stages a server-side secret (never trust the
// client), confirm validates + issues recovery codes, disable needs password
// + a live code so a hijacked session can't strip MFA.
package handler

import (
	"errors"
	"net/http"

	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/pquerna/otp/totp"
)

type MFAHandler struct{ svc *auth.Service }

func NewMFAHandler(svc *auth.Service) *MFAHandler { return &MFAHandler{svc: svc} }

func (h *MFAHandler) Setup(w http.ResponseWriter, r *http.Request) {
	userID := middleware.UserID(r.Context())
	if sec, _ := h.svc.MFASecret(r.Context(), userID); sec != "" {
		writeErr(w, 409, "mfa_already_enabled", "mfa already enabled on this account")
		return
	}
	u, err := h.svc.GetUser(r.Context(), userID)
	if err != nil {
		writeErr(w, 404, "user_not_found", "user not found")
		return
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: "OGA9", AccountName: u.Email})
	if err != nil {
		writeErr(w, 500, "internal_error", "totp generation failed")
		return
	}
	if err := h.svc.StageMFASecret(r.Context(), userID, key.Secret()); err != nil {
		writeErr(w, 500, "internal_error", "mfa staging failed")
		return
	}
	writeData(w, 200, map[string]any{
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
	if err := decode(w, r, &req); err != nil || req.Code == "" {
		writeErr(w, 400, "validation_failed", "code required")
		return
	}
	secret, err := h.svc.PendingMFASecret(r.Context(), userID)
	if err != nil || secret == "" {
		writeErr(w, 400, "mfa_not_staged", "call mfa/setup first")
		return
	}
	if !totp.Validate(req.Code, secret) {
		writeErr(w, 400, "invalid_code", "TOTP code invalid")
		return
	}
	codes, err := h.svc.SetupMFA(r.Context(), userID, secret)
	if err != nil {
		writeErr(w, 500, "internal_error", "mfa save failed")
		return
	}
	h.svc.ClearPendingMFA(r.Context(), userID)
	// recovery codes are returned ONCE — store them offline.
	writeData(w, 200, map[string]any{"mfa_enabled": true, "recovery_codes": codes})
}

type mfaDisableReq struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (h *MFAHandler) Disable(w http.ResponseWriter, r *http.Request) {
	var req mfaDisableReq
	if err := decode(w, r, &req); err != nil || req.Password == "" || req.Code == "" {
		writeErr(w, 400, "validation_failed", "password and code required")
		return
	}
	err := h.svc.DisableMFA(r.Context(), middleware.UserID(r.Context()), req.Password, req.Code)
	if errors.Is(err, auth.ErrBadCredentials) {
		writeErr(w, 401, "invalid_credentials", "password incorrect")
		return
	}
	if errors.Is(err, auth.ErrMFACode) {
		writeErr(w, 401, "invalid_mfa_code", "code incorrect")
		return
	}
	if errors.Is(err, auth.ErrMFANotEnabled) {
		writeErr(w, 409, "mfa_not_enabled", "mfa not enabled")
		return
	}
	if err != nil {
		writeErr(w, 500, "internal_error", "disable failed")
		return
	}
	writeData(w, 200, map[string]any{"mfa_enabled": false})
}
