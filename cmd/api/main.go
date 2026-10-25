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
	sentryhttp "github.com/getsentry/sentry-go/http"
	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/config"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/events"
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
	if cfg.GlitchTipDSN != "" {
		// capture panics to GlitchTip; Repanic lets chi Recoverer still 500
		r.Use(sentryhttp.New(sentryhttp.Options{Repanic: true}).Handle)
	}
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
	entSvc := entitlements.NewService(pool, vk)
	billSvc := billing.NewService(pool, cfg.LicenseSignKey, cfg.WebhookSecret)
	for taskType, h := range tasks.Handlers(tasks.Deps{
		Mail: mail, Pool: pool, RedisAddr: cfg.ValkeyAddr, Billing: billSvc, Queue: queue,
	}) {
		mux.HandleFunc(taskType, h)
	}
	authSvc := auth.NewService(pool, vk, cfg.JWTSecret, cfg.SessionTTL, cfg.DataKey)
	authH := handler.NewAuthHandler(authSvc, queue, cfg.JWTSecret, cfg.AppURL)
	mfaH := handler.NewMFAHandler(authSvc)
	providers := make(map[string]auth.ProviderConfig, 2)
	for name, p := range cfg.OAuthProviders() {
		providers[name] = auth.ProviderConfig{
			ClientID: p.ClientID, ClientSecret: p.ClientSecret,
			AuthURL: p.AuthURL, TokenURL: p.TokenURL, UserinfoURL: p.UserinfoURL,
		}
	}
	oauthH := handler.NewOAuthHandler(authSvc, vk, providers, cfg.BaseURL, cfg.AppURL, queue, cfg.JWTSecret)
	orgSvc := tenant.NewService(pool, vk)
	orgH := handler.NewOrgHandler(orgSvc)

	// §06/§07 app framework + entitlements/billing
	appsH := handler.NewAppsHandler(orgSvc, entSvc)
	billH := handler.NewBillingHandler(billSvc, orgSvc, entSvc, queue)

	// §05 outbox dispatcher: lease-locked claim + consumer-log dedupe.
	dispatcher := events.NewDispatcher(pool, vk)
	dispatcher.Register("entity_links", events.EntityLinksConsumer)
	dispatcher.Register("entitlements_cache", entSvc.InvalidateConsumer)
	mux.HandleFunc(events.TypeDispatchOutbox, dispatcher.ProcessTask)
	if _, err := scheduler.Register(
		"@every "+cfg.OutboxInterval.String(),
		asynq.NewTask(events.TypeDispatchOutbox, nil, asynq.Queue("critical")),
	); err != nil {
		log.Fatalf("outbox schedule: %v", err)
	}
	// §08 recurring jobs: DLQ re-drive (default tier per §08 seeded table) +
	// the scheduler:tick cron driver (sweeps + queue-depth metric).
	for typ, iv := range map[string]time.Duration{
		tasks.TypeRetryDead:     cfg.RetryDeadInterval,
		tasks.TypeSchedulerTick: cfg.SchedulerTickInterval,
	} {
		if _, err := scheduler.Register(
			"@every "+iv.String(),
			asynq.NewTask(typ, nil, asynq.Queue("default")),
		); err != nil {
			log.Fatalf("schedule %s: %v", typ, err)
		}
	}
	// §07 lifecycle sweep — critical tier (billing-adjacent, per catalog).
	if _, err := scheduler.Register(
		"@every "+cfg.BillingSweepInterval.String(),
		asynq.NewTask(tasks.TypeSubscriptionSweep, nil, asynq.Queue("critical")),
	); err != nil {
		log.Fatalf("schedule %s: %v", tasks.TypeSubscriptionSweep, err)
	}

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

		// §07 self-hosted callback + provider webhook — no session auth;
		// the signed material / HMAC signature is the credential (Rules 9/29).
		r.With(middleware.RateLimit(vk, "license", 30, time.Minute)).Post("/license/validate", billH.ValidateLicense)
		r.With(middleware.RateLimit(vk, "webhook", 60, time.Minute)).Post("/webhooks/billing/{provider}", billH.Webhook)

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

			// §06/§07 platform surfaces — platform.* nodes resolve via
			// NULL-tenant roles only (RequirePlatformNode).
			r.With(middleware.RequirePlatformNode(pool, "platform.apps.view")).Get("/platform/apps", appsH.Registry)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.view")).Get("/platform/plans", billH.Plans)
			r.With(middleware.RequirePlatformNode(pool, "platform.licenses.issue")).Post("/platform/licenses", billH.IssueLicense)
			// §07 billing — tenant resolves server-side from the caller's
			// org.admin.billing node grants (tenantFor in the handler, Rule 24).
			r.Get("/me/entitlements", billH.Entitlements)
			r.Get("/billing/subscription", billH.Subscription)
			r.Post("/billing/subscription", billH.CreateSubscription)
			r.Post("/billing/change-plan", billH.ChangePlan)
			r.Post("/billing/cancel", billH.Cancel)
			r.Get("/billing/invoices", billH.Invoices)

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
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Get("/orgs/{org_id}/apps", appsH.OrgApps)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Post("/orgs/{org_id}/apps/{app_id}", appsH.Activate)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Delete("/orgs/{org_id}/apps/{app_id}", appsH.Deactivate)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/positions", orgH.ListPositions)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/terms", orgH.ListTerms)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/appointments", orgH.ListAppointments)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/units/{unit_id}/members", orgH.ListMembers)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.positions.manage")).Post("/orgs/{org_id}/positions", orgH.CreatePosition)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.terms.manage")).Post("/orgs/{org_id}/terms", orgH.CreateTerm)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.terms.manage")).Post("/orgs/{org_id}/terms/{term_id}/close", orgH.CloseTerm)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.appointments.manage")).Post("/orgs/{org_id}/appointments", orgH.Appoint)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.appointments.manage")).Delete("/orgs/{org_id}/appointments/{appointment_id}", orgH.EndAppointment)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.committees.manage")).Post("/orgs/{org_id}/units/{unit_id}/members", orgH.AddCommitteeMember)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.committees.manage")).Delete("/orgs/{org_id}/units/{unit_id}/members/{cm_id}", orgH.RemoveCommitteeMember)
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
