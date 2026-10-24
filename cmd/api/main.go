// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// Ref: oga9-engineering/sections/08-job-queue-asynq.md
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/config"
	"github.com/jedafe/oga9/internal/handler"
	"github.com/jedafe/oga9/internal/mailer"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tasks"
	"github.com/jedafe/oga9/internal/tenant"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	if cfg.GlitchTipDSN != "" {
		if err := sentry.Init(sentry.ClientOptions{Dsn: cfg.GlitchTipDSN, Environment: cfg.Env}); err != nil {
			log.Printf("glitchtip init failed (continuing without): %v", err)
		}
		defer sentry.Flush(2 * time.Second)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("postgres: %v", err)
	}
	defer pool.Close()

	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{cfg.ValkeyAddr}})
	if err != nil {
		log.Fatalf("valkey: %v", err)
	}
	defer vk.Close()

	// Asynq server + scheduler (3-tier: critical/default/low — §08)
	srv := asynq.NewServer(
		asynq.RedisClientOpt{Addr: cfg.ValkeyAddr},
		asynq.Config{Queues: map[string]int{"critical": 6, "default": 3, "low": 1}},
	)
	mux := asynq.NewServeMux()
	go func() {
		if err := srv.Run(mux); err != nil {
			log.Printf("asynq server: %v", err)
		}
	}()

	scheduler := asynq.NewScheduler(
		asynq.RedisClientOpt{Addr: cfg.ValkeyAddr},
		&asynq.SchedulerOpts{},
	)
	go func() {
		if err := scheduler.Run(); err != nil {
			log.Printf("asynq scheduler: %v", err)
		}
	}()

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(chimiddleware.Recoverer)

	r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
		if err := pool.Ping(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if err := vk.Do(r.Context(), vk.B().Ping().Build()).Error(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	// §02 identity: auth service + handlers
	mail := mailer.New(cfg.SMTPHost, cfg.SMTPPort, cfg.MailFrom, cfg.SMTPUser, cfg.SMTPPass)
	queue := asynq.NewClient(asynq.RedisClientOpt{Addr: cfg.ValkeyAddr})
	defer queue.Close()
	for taskType, h := range tasks.Handlers(mail) {
		mux.HandleFunc(taskType, h)
	}
	authSvc := auth.NewService(pool, vk, cfg.JWTSecret, cfg.SessionTTL, cfg.DataKey)
	authH := handler.NewAuthHandler(authSvc, queue, cfg.JWTSecret, cfg.BaseURL)
	mfaH := handler.NewMFAHandler(authSvc)
	providers := make(map[string]auth.ProviderConfig, 2)
	for name, p := range cfg.OAuthProviders() {
		providers[name] = auth.ProviderConfig{
			ClientID: p.ClientID, ClientSecret: p.ClientSecret,
			AuthURL: p.AuthURL, TokenURL: p.TokenURL, UserinfoURL: p.UserinfoURL,
		}
	}
	oauthH := handler.NewOAuthHandler(authSvc, vk, providers, cfg.BaseURL, queue, cfg.JWTSecret)
	orgSvc := tenant.NewService(pool, vk)
	orgH := handler.NewOrgHandler(orgSvc)

	r.Use(middleware.ResolveIP(cfg.TrustedProxies))
	r.Route("/api/v1", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"data":{"service":"oga9","version":"0.0.1"}}`))
		})

		// public auth — Valkey fixed-window rate limits (ADR-020 baseline)
		r.With(middleware.RateLimit(vk, "register", 5, time.Minute)).Post("/auth/register", authH.Register)
		r.With(middleware.RateLimit(vk, "login", 10, time.Minute)).Post("/auth/login", authH.Login)
		r.Post("/auth/verify/{token}", authH.VerifyEmail)
		r.With(middleware.RateLimit(vk, "resend", 3, time.Minute)).Post("/auth/verify/resend", authH.ResendVerification)
		r.With(middleware.RateLimit(vk, "forgot", 3, time.Minute)).Post("/auth/password/forgot", authH.ForgotPassword)
		r.With(middleware.RateLimit(vk, "reset", 5, time.Minute)).Post("/auth/password/reset", authH.ResetPassword)
		r.With(middleware.RateLimit(vk, "oauth", 20, time.Minute)).Get("/auth/oauth/{provider}/start", oauthH.Start)
		r.With(middleware.RateLimit(vk, "oauth", 20, time.Minute)).Get("/auth/oauth/{provider}/callback", oauthH.Callback)
		r.With(middleware.RateLimit(vk, "mfa", 10, time.Minute)).Post("/auth/mfa/complete", mfaH.Complete)
		r.With(middleware.RateLimit(vk, "directory", 30, time.Minute)).Get("/directory/organizations", orgH.Directory)

		// authed — session JWT or mfa_token both reach the mfa enrollment routes
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuthOrMFAGate(authSvc, cfg.JWTSecret))
			r.Post("/auth/mfa/setup", mfaH.Setup)
			r.Post("/auth/mfa/confirm", mfaH.Confirm)
		})

		// authed — session JWT only
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuth(authSvc, cfg.JWTSecret))
			r.Post("/auth/logout", authH.Logout)
			r.Get("/me", authH.Me)
			r.Get("/me/sessions", authH.Sessions)
			r.Delete("/me/sessions/{session_id}", authH.RevokeSession)
			r.Post("/auth/mfa/disable", mfaH.Disable)

			// §04/§10 tenancy + organization core
			r.Get("/me/organizations", orgH.MyOrgs)
			r.Get("/org-templates", orgH.Templates)
			r.Post("/orgs", orgH.Create)
			r.With(middleware.RateLimit(vk, "invite_accept", 10, time.Minute)).Post("/invitations/{token}/accept", orgH.Accept)
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireOrgCtx(orgSvc))
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}", orgH.Get)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Patch("/orgs/{org_id}", orgH.Patch)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/structure", orgH.Structure)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/units", orgH.Units)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.structure.manage")).Post("/orgs/{org_id}/units", orgH.CreateUnit)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.structure.manage")).Patch("/orgs/{org_id}/units/{unit_id}", orgH.UpdateUnit)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.structure.manage")).Delete("/orgs/{org_id}/units/{unit_id}", orgH.ArchiveUnit)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Get("/orgs/{org_id}/settings", orgH.Settings)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Patch("/orgs/{org_id}/settings", orgH.SetSetting)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.invite")).Get("/orgs/{org_id}/invitations", orgH.Invitations)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.invite")).Post("/orgs/{org_id}/invitations", orgH.Invite)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.invite")).Delete("/orgs/{org_id}/invitations/{invitation_id}", orgH.RevokeInvitation)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.branding")).Patch("/orgs/{org_id}/branding", orgH.Branding)
			})
		})
	})

	httpSrv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.APIPort),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // WS endpoints override via hijack
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("api listening on :%d (env=%s)", cfg.APIPort, cfg.Env)
		if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("shutting down...")
	cancel()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	srv.Shutdown()
	scheduler.Shutdown()
	httpSrv.Shutdown(shutdownCtx)
}
