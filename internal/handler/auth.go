// Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tasks"
)

type AuthHandler struct {
	svc    *auth.Service
	queue  *asynq.Client
	secret string
}

func NewAuthHandler(svc *auth.Service, q *asynq.Client, secret string) *AuthHandler {
	return &AuthHandler{svc: svc, queue: q, secret: secret}
}

// enqueueMail is fire-and-forget (Rule 8): queue the mail task, never block.
func (h *AuthHandler) enqueueMail(ctx context.Context, taskType, to, subject, body string) {
	t, err := tasks.NewEmailTask(taskType, tasks.SendEmailPayload{To: to, Subject: subject, Body: body})
	if err != nil {
		return
	}
	_, _ = h.queue.EnqueueContext(ctx, t)
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
func decode(w http.ResponseWriter, r *http.Request, v any) error {
	// 1 MiB cap on JSON bodies — auth endpoints never need more.
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v)
}

type registerReq struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
}

func (h *AuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := decode(w, r, &req); err != nil || req.Email == "" || req.Password == "" {
		writeErr(w, 400, "validation_failed", "email and password required")
		return
	}
	u, verifyToken, err := h.svc.Register(r.Context(), req.Email, req.Password, req.DisplayName)
	if errors.Is(err, auth.ErrEmailTaken) {
		writeErr(w, 409, "email_taken", "email already registered")
		return
	}
	if errors.Is(err, auth.ErrWeakPassword) {
		writeErr(w, 400, "weak_password", "min 8 chars with a letter and a digit")
		return
	}
	if err != nil {
		writeErr(w, 500, "internal_error", "registration failed")
		return
	}
	h.enqueueMail(r.Context(), tasks.TypeSendVerification, u.Email,
		"Verify your OGA9 account", "Verify: "+verifyToken)
	writeData(w, 201, map[string]any{"user_id": u.ID, "email": u.Email, "display_name": u.DisplayName})
}

type loginReq struct {
	Email       string `json:"email"`
	Password    string `json:"password"`
	MFACode     string `json:"mfa_code"`
	DeviceLabel string `json:"device_label"`
}

func (h *AuthHandler) Login(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decode(w, r, &req); err != nil {
		writeErr(w, 400, "validation_failed", "email+password required")
		return
	}
	ip := middleware.ClientIPFrom(r.Context())
	token, sessionID, err := h.svc.Login(r.Context(), req.Email, req.Password, req.MFACode, req.DeviceLabel, ip, r.UserAgent())
	if errors.Is(err, auth.ErrBadCredentials) {
		writeErr(w, 401, "invalid_credentials", "email or password incorrect")
		return
	}
	if errors.Is(err, auth.ErrUserSuspended) {
		writeErr(w, 403, "user_suspended", "account suspended")
		return
	}
	if errors.Is(err, auth.ErrMFARequired) {
		writeErr(w, 403, "mfa_required", "mfa code required")
		return
	}
	if errors.Is(err, auth.ErrMFACode) {
		writeErr(w, 401, "invalid_mfa_code", "mfa code incorrect")
		return
	}
	if err != nil {
		writeErr(w, 500, "internal_error", "login failed")
		return
	}
	writeData(w, 200, map[string]any{"token": token, "session_id": sessionID})
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.Logout(r.Context(), middleware.SessionJTI(r.Context())); err != nil {
		writeErr(w, 500, "internal_error", "logout failed")
		return
	}
	writeData(w, 200, map[string]any{"logged_out": true})
}

func (h *AuthHandler) VerifyEmail(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	if token == "" {
		var b struct{ Token string }
		decode(w, r, &b)
		token = b.Token
	}
	if err := h.svc.VerifyEmail(r.Context(), token); err != nil {
		writeErr(w, 400, "invalid_token", "verification link invalid or expired")
		return
	}
	writeData(w, 200, map[string]any{"verified": true})
}

type forgotReq struct {
	Email string `json:"email"`
}

func (h *AuthHandler) ForgotPassword(w http.ResponseWriter, r *http.Request) {
	var req forgotReq
	if err := decode(w, r, &req); err != nil || req.Email == "" {
		writeErr(w, 400, "validation_failed", "email required")
		return
	}
	// Always 200 — don't leak account existence (§02)
	uid, _ := h.svc.UserIDByEmail(r.Context(), req.Email)
	if uid != "" {
		token, _ := auth.SignActionToken(h.secret, uid, "password_reset", time.Hour)
		h.enqueueMail(r.Context(), tasks.TypeSendPasswordReset, req.Email,
			"OGA9 password reset", "Reset: "+token)
	}
	writeData(w, 200, map[string]any{"sent": true})
}

type resetReq struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

func (h *AuthHandler) ResetPassword(w http.ResponseWriter, r *http.Request) {
	var req resetReq
	if err := decode(w, r, &req); err != nil || req.Token == "" || req.NewPassword == "" {
		writeErr(w, 400, "validation_failed", "token and new password required")
		return
	}
	if err := h.svc.ResetPassword(r.Context(), req.Token, req.NewPassword); err != nil {
		writeErr(w, 400, "invalid_token", "reset link invalid, used, or expired")
		return
	}
	writeData(w, 200, map[string]any{"reset": true})
}

func (h *AuthHandler) Me(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.GetUser(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		writeErr(w, 404, "user_not_found", "user not found")
		return
	}
	writeData(w, 200, u)
}

func (h *AuthHandler) Sessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := h.svc.ListSessions(r.Context(), middleware.UserID(r.Context()))
	jti := middleware.SessionJTI(r.Context())
	for i := range sessions {
		sessions[i].Current = sessions[i].JTI == jti
	}
	if err != nil {
		writeErr(w, 500, "internal_error", "could not list sessions")
		return
	}
	writeData(w, 200, sessions)
}

func (h *AuthHandler) RevokeSession(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "session_id"))
	if err != nil {
		writeErr(w, 400, "validation_failed", "bad session id")
		return
	}
	if err := h.svc.RevokeSession(r.Context(), middleware.UserID(r.Context()), id); err != nil {
		writeErr(w, 500, "internal_error", "revoke failed")
		return
	}
	writeData(w, 200, map[string]any{"revoked": true})
}

// ResendVerification re-sends the verify email for unverified accounts.
// Always 200 — don't leak account existence (same contract as forgot).
func (h *AuthHandler) ResendVerification(w http.ResponseWriter, r *http.Request) {
	var req forgotReq
	if err := decode(w, r, &req); err != nil || req.Email == "" {
		writeErr(w, 400, "validation_failed", "email required")
		return
	}
	if uid, _ := h.svc.UserIDByEmail(r.Context(), req.Email); uid != "" {
		if u, _ := h.svc.GetUser(r.Context(), uuid.MustParse(uid)); u != nil && !u.IsVerified {
			token, _ := auth.SignActionToken(h.secret, uid, "verify_email", 24*time.Hour)
			h.enqueueMail(r.Context(), tasks.TypeSendVerification, u.Email,
				"Verify your OGA9 account", "Verify: "+token)
		}
	}
	writeData(w, 200, map[string]any{"sent": true})
}
