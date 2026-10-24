// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
// TOTP MFA — setup generates secret + otpauth URL, confirm validates code.
package handler

import (
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
	writeData(w, 200, map[string]any{
		"secret": key.Secret(), "otpauth_url": key.URL(),
		"note": "confirm with POST /auth/mfa/confirm + code to activate",
	})
}

type mfaConfirmReq struct {
	Secret string `json:"secret"`
	Code   string `json:"code"`
}

func (h *MFAHandler) Confirm(w http.ResponseWriter, r *http.Request) {
	var req mfaConfirmReq
	if err := decode(w, r, &req); err != nil || req.Secret == "" || req.Code == "" {
		writeErr(w, 400, "validation_failed", "secret and code required")
		return
	}
	if !totp.Validate(req.Code, req.Secret) {
		writeErr(w, 400, "invalid_code", "TOTP code invalid")
		return
	}
	if err := h.svc.SetupMFA(r.Context(), middleware.UserID(r.Context()), req.Secret); err != nil {
		writeErr(w, 500, "internal_error", "mfa save failed")
		return
	}
	writeData(w, 200, map[string]any{"mfa_enabled": true})
}
