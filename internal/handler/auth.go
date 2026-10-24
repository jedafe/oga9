// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
package handler

import (
	"encoding/json"
	"net"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/mailer"
	"github.com/jedafe/oga9/internal/middleware"
)

type AuthHandler struct {
	svc    *auth.Service
	mailer *mailer.Mailer
	secret string
}

func NewAuthHandler(svc *auth.Service, m *mailer.Mailer, secret string) *AuthHandler {
	return &AuthHandler{svc: svc, mailer: m, secret: secret}
}

func writeData(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"data": data})
}
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"code": code, "message": msg, "status": status},
	})
}
func decode(r *http.Request, v any) error { return json.NewDecoder(r.Body).Decode(v) }

type registerReq struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}
func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := decode(r, &req); err != nil || req.Email == "" || req.Password == "" {
		writeErr(w, 400, "validation_failed", "email and password required"); return
	}
	u, verifyToken, err := h.svc.Register(r.Context(), req.Email, req.Password, req.DisplayName)
	if errors.Is(err, auth.ErrEmailTaken) { writeErr(w, 409, "email_taken", "email already registered"); return }
	if err != nil { writeErr(w, 500, "internal_error", "registration failed"); return }
	h.mailer.Send(u.Email, "Verify your OGA9 account",
		"Verify: "+verifyToken) // TODO(§02): branded template + link
	writeData(w, 201, map[string]any{"user_id": u.ID, "email": u.Email, "display_name": u.DisplayName})
}

type loginReq struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DeviceLabel string `json:"device_label"`
}
func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decode(r, &req); err != nil { writeErr(w, 400, "validation_failed", "email+password required"); return }
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	token, sessionID, err := h.svc.Login(r.Context(), req.Email, req.Password, req.DeviceLabel, ip, r.UserAgent())
	if errors.Is(err, auth.ErrBadCredentials) { writeErr(w, 401, "invalid_credentials", "email or password incorrect"); return }
	if errors.Is(err, auth.ErrUserSuspended) { writeErr(w, 403, "user_suspended", "account suspended"); return }
	if err != nil { writeErr(w, 500, "internal_error", "login failed"); return }
	writeData(w, 200, map[string]any{"token": token, "session_id": sessionID})
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Logout(r.Context(), middleware.SessionJTI(r.Context())); err != nil {
		writeErr(w, 500, "internal_error", "logout failed"); return
	}
	writeData(w, 200, map[string]any{"logged_out": true})
}

func (h *AuthHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" { var b struct{ Token string }; decode(r, &b); token = b.Token }
	if err := h.svc.VerifyEmail(r.Context(), token); err != nil {
		writeErr(w, 400, "invalid_token", "verification link invalid or expired"); return
	}
	writeData(w, 200, map[string]any{"verified": true})
}

type forgotReq struct{ Email string `json:"email"` }
func (h *AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotReq
	if err := decode(r, &req); err != nil || req.Email == "" {
		writeErr(w, 400, "validation_failed", "email required"); return
	}
	// Always 200 — don't leak account existence (§02)
	token, _ := auth.SignActionToken(h.secret, "lookup:"+req.Email, "password_reset", 3600000000000)
	h.mailer.Send(req.Email, "OGA9 password reset", "Reset: "+token)
	writeData(w, 200, map[string]any{"sent": true})
}

type resetReq struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}
func (h *AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetReq
	if err := decode(r, &req); err != nil || req.Token == "" || req.NewPassword == "" {
		writeErr(w, 400, "validation_failed", "token and new password required"); return
	}
	// TODO(§02): consume single-use token + update password_hash via svc.ResetPassword
	writeData(w, 501, map[string]any{"implemented": false, "note": "password reset lands with token-consume task"})
}

func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.GetUser(r.Context(), middleware.UserID(r.Context()))
	if err != nil { writeErr(w, 404, "user_not_found", "user not found"); return }
	writeData(w, 200, u)
}

func (h *AuthHandler) Sessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.svc.ListSessions(r.Context(), middleware.UserID(r.Context()))
	if err != nil { writeErr(w, 500, "internal_error", "could not list sessions"); return }
	writeData(w, 200, sessions)
}

func (h *AuthHandler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "session_id"))
	if err != nil { writeErr(w, 400, "validation_failed", "bad session id"); return }
	if err := h.svc.RevokeSession(r.Context(), middleware.UserID(r.Context()), id); err != nil {
		writeErr(w, 500, "internal_error", "revoke failed"); return
	}
	writeData(w, 200, map[string]any{"revoked": true})
}
