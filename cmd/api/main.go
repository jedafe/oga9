// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// Ref: oga9-engineering/sections/08-job-queue-asynq.md
package main

import (
	"context"
	"fmt"
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
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/activities"
	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/comms"
	"github.com/jedafe/oga9/internal/config"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/finance"
	"github.com/jedafe/oga9/internal/handler"
	"github.com/jedafe/oga9/internal/license"
	"github.com/jedafe/oga9/internal/mailer"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/obs"
	"github.com/jedafe/oga9/internal/platform"
	"github.com/jedafe/oga9/internal/realtime"
	"github.com/jedafe/oga9/internal/storage"
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
	// resolved client IP must land in ctx BEFORE obs logging and every
	// rate-limit bucket — otherwise the proxy's addr is what gets counted.
	r.Use(middleware.ResolveIP(cfg.TrustedProxies))
	// §13.05: structured access log + per-route latency reservoir.
	r.Use(obs.Metrics)
	r.Use(obs.RequestLog)
	// §13.03: global read tier — bounds GET/HEAD per IP; stricter
	// per-endpoint buckets (auth/write/upload/payment) stack on top.
	r.Use(middleware.RateLimitReads(vk, "read", 240, time.Minute))

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
	orgSvc := tenant.NewService(pool, vk)
	finSvc := finance.NewService(pool, vk)
	// §07: limit.members.active gate at membership activation (Rule 25).
	orgSvc.SetEntitlements(entSvc)
	// ADR-014: approved→registered requires settled registration dues —
	// the gate lives in finance so members never reads finance tables.
	orgSvc.SetRegistrationGate(finSvc.RegistrationPending)
	taskDeps := tasks.Deps{
		Mail: mail, Pool: pool, RedisAddr: cfg.ValkeyAddr, Valkey: vk, Billing: billSvc, Queue: queue, Tenant: orgSvc,
		Finance: finSvc,
	}
	actSvc := activities.NewService(pool)
	actSvc.SetFineHook(finSvc.AssessFine)
	actSvc.SetFineRevokeHook(finSvc.RevokeFine)
	// reverse seam: finance validates payments.activity_id through the
	// activities module (Rule 26 — no cross-module table reads).
	finSvc.SetActivityGate(actSvc.ActivityInOrg)
	actH := handler.NewActivitiesHandler(actSvc, orgSvc)
	commsSvc := comms.NewService(pool)
	commsSvc.SetStore(storage.New(cfg.UploadsDir))
	commsSvc.SetLimitFn(entSvc.Limit)
	commsSvc.SetUploadFallbackMB(cfg.UploadMaxMB)
	commsH := handler.NewCommsHandler(commsSvc, queue, orgSvc)
	chatSvc := chat.NewService(pool, vk)
	chatSvc.SetDocGate(commsSvc.DocInOrg)
	chatSvc.SetActivityGate(actSvc.ActivityInOrg)
	chatSvc.SetVoiceProvider(cfg.LiveKitURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret, cfg.VoiceJoinWindowSec)
	chatH := handler.NewChatHandler(chatSvc, orgSvc)
	wsGw := realtime.NewGateway(chatSvc, vk, cfg.BaseDomain, cfg.Env != "production")
	// §15 platform control plane — provision pipeline + operator routes.
	platSvc := platform.NewService(pool)
	platSvc.SetStore(storage.New(cfg.UploadsDir)) // §13.06 async tenant exports
	platSvc.SetOrgBootstrap(func(ctx context.Context, userID uuid.UUID, name, slug, templateSlug, orgType, country, city, billingEmail string, requestedTenant uuid.UUID) (uuid.UUID, error) {
		org, err := orgSvc.CreateOrg(ctx, userID, name, slug, templateSlug, orgType, country, city, billingEmail, requestedTenant, entSvc)
		if err != nil {
			return uuid.Nil, err
		}
		return org.ID, nil
	})
	platH := handler.NewPlatformHandler(platSvc, queue)
	taskDeps.Activities = actSvc
	taskDeps.Comms = commsSvc
	taskDeps.Chat = chatSvc
	taskDeps.Platform = platSvc
	taskDeps.VoiceRoomMaxAgeHours = cfg.VoiceRoomMaxAgeHours
	// §14.03: offline license check (signed file + grace, Rule 29).
	licV, licErr := license.New(cfg.LicensePublicKey, cfg.LicenseFile, cfg.LicenseOfflineGraceDays)
	if licErr != nil {
		log.Fatalf("license: %v", licErr)
	}
	if st, f, err := licV.Check(); err != nil {
		log.Printf("license: %v (status=%s)", err, st)
	} else {
		switch st {
		case license.StatusGrace:
			log.Printf("license: expired %s — offline grace %dd (deployment_id=%s)", f.ExpiresAt, cfg.LicenseOfflineGraceDays, f.DeploymentID)
		case license.StatusExpired, license.StatusInvalid:
			log.Printf("license: %s — deployment degraded per §09 matrix", st)
		default:
			log.Printf("license: %s", st)
		}
	}
	taskDeps.License = licV
	tasks.RegisterSweeps(taskDeps)
	for taskType, h := range tasks.Handlers(taskDeps) {
		mux.HandleFunc(taskType, h)
	}
	authSvc := auth.NewService(pool, vk, cfg.JWTSecret, cfg.SessionTTL, cfg.DataKey)
	authH := handler.NewAuthHandler(authSvc, queue, cfg.JWTSecret, cfg.AppURL, orgSvc)
	// §15 welcome mail: auth mints a 48h password_reset action token; the
	// owner picks their own credentials (no plaintext password is set).
	platSvc.SetTrialDays(int64(cfg.TrialDays))
	// §15 support sessions: platform never writes user_sessions itself —
	// auth mints the 30min operator token linked to the support row.
	platSvc.SetSessionMinter(func(ctx context.Context, supportSessionID, targetUserID uuid.UUID, ttl time.Duration, ip, ua string) (string, uuid.UUID, error) {
		return authSvc.CreateSupportSession(ctx, supportSessionID, targetUserID, ttl, ip, ua)
	})
	platSvc.SetWelcomeMail(func(ctx context.Context, email, name string, uid uuid.UUID) error {
		tok := authSvc.MintActionToken(uid, "password_reset", 48*time.Hour)
		link := fmt.Sprintf("%s/auth/reset?token=%s", cfg.AppURL, tok)
		return mail.Send(email, "Welcome to OGA9",
			fmt.Sprintf("Hello %s,\n\nYour organization has been provisioned. Set your password here:\n%s\n\n(The link expires in 48 hours.)", name, link))
	})
	mfaH := handler.NewMFAHandler(authSvc, orgSvc)
	providers := make(map[string]auth.ProviderConfig, 2)
	for name, p := range cfg.OAuthProviders() {
		providers[name] = auth.ProviderConfig{
			ClientID: p.ClientID, ClientSecret: p.ClientSecret,
			AuthURL: p.AuthURL, TokenURL: p.TokenURL, UserinfoURL: p.UserinfoURL,
		}
	}
	oauthH := handler.NewOAuthHandler(authSvc, vk, providers, cfg.BaseURL, cfg.AppURL, queue, cfg.JWTSecret, orgSvc)
	orgH := handler.NewOrgHandler(orgSvc, entSvc)
	finH := handler.NewFinanceHandler(finSvc, orgSvc)

	// §06/§07 app framework + entitlements/billing
	appsH := handler.NewAppsHandler(orgSvc, entSvc)
	billH := handler.NewBillingHandler(billSvc, orgSvc, entSvc, queue)
	memberH := handler.NewMemberHandler(orgSvc, queue)

	// §05 outbox dispatcher: lease-locked claim + consumer-log dedupe.
	dispatcher := events.NewDispatcher(pool, vk)
	dispatcher.Register("entity_links", events.EntityLinksConsumer)
	dispatcher.Register("entitlements_cache", entSvc.InvalidateConsumer)
	dispatcher.Register("dues_standing", finSvc.DuesStandingConsumer)
	dispatcher.Register("registration_obligations", finSvc.RegistrationObligationConsumer)
	dispatcher.Register("obligation_generation", tasks.DuesSchedulePublishedConsumer(queue))
	// §13/§14: approved minutes archive to documents + entity_links edges.
	dispatcher.Register("minutes_archive", commsSvc.MinutesArchivedConsumer)
	// §14: member-targeted events (overdue items, dues flags) → in_app
	// notifications gated on preferences.
	dispatcher.Register("member_notify", commsSvc.EventNotifyConsumer)
	// §15: subscription/provision changes recompute countable usage meters.
	dispatcher.Register("usage_recompute", billSvc.UsageRecomputeConsumer)
	// §15: tenant status transitions write through to the oga9:tenant_status
	// cache so suspension takes effect without waiting out the 60s TTL.
	dispatcher.Register("tenant_status_cache", orgSvc.TenantStatusCacheConsumer)
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
		r.With(middleware.RateLimit(vk, "directory_apply", 10, time.Minute)).Post("/directory/orgs/{slug}/apply", memberH.PublicApply)
		r.With(middleware.RateLimit(vk, "directory_apply", 30, time.Minute)).Get("/directory/orgs/{slug}/apply", memberH.ApplyForm)
		r.With(middleware.RateLimit(vk, "directory", 60, time.Minute)).Get("/directory/orgs/{slug}/docs", commsH.PublicDocs)

		// §07 self-hosted callback + provider webhook — no session auth;
		// the signed material / HMAC signature is the credential (Rules 9/29).
		r.With(middleware.RateLimit(vk, "license", 30, time.Minute)).Post("/license/validate", billH.ValidateLicense)
		r.With(middleware.RateLimit(vk, "webhook", 300, time.Minute)).Post("/webhooks/billing/{provider}", billH.Webhook)

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
			r.With(middleware.RequirePlatformNode(pool, "platform.licenses.issue")).Get("/platform/licenses/{deployment_id}/file", billH.LicenseFile)
			// §15 control plane
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Get("/platform/tenants", platH.Tenants)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Get("/platform/tenants/{tenant_id}", platH.Tenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Get("/platform/tenants/{tenant_id}/export", platH.ExportTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage"), middleware.RateLimit(vk, "platform", 10, time.Minute)).Post("/platform/tenants/{tenant_id}/export", platH.StartTenantExport)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Get("/platform/tenants/{tenant_id}/exports/{export_id}", platH.DownloadTenantExport)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage"), middleware.RateLimit(vk, "platform", 30, time.Minute)).Post("/platform/tenants", platH.CreateTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Post("/platform/tenants/{tenant_id}/suspend", platH.SuspendTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Post("/platform/tenants/{tenant_id}/reinstate", platH.ReinstateTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.subscriptions.manage")).Get("/platform/subscriptions", platH.Subscriptions)
			r.With(middleware.RequirePlatformNode(pool, "platform.subscriptions.manage")).Post("/platform/subscriptions/{subscription_id}/override", platH.OverrideSubscription)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.manage")).Post("/platform/plans", platH.UpsertPlan)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.manage")).Patch("/platform/plans/{slug}", platH.UpsertPlan)
			r.With(middleware.RequirePlatformNode(pool, "platform.deployments.manage")).Get("/platform/deployments", platH.Deployments)
			r.With(middleware.RequirePlatformNode(pool, "platform.deployments.manage")).Post("/platform/deployments", platH.CreateDeployment)
			r.With(middleware.RequirePlatformNode(pool, "platform.support.impersonate"), middleware.RateLimit(vk, "platform", 10, time.Minute)).Post("/platform/support/impersonate", platH.Impersonate)
			r.With(middleware.RequirePlatformNode(pool, "platform.support.impersonate")).Post("/platform/support/impersonate/{session_id}/end", platH.EndImpersonation)
			r.Get("/me/support-sessions", platH.MySupportSessions) // §15 banner — tenant-visible
			r.With(middleware.RequirePlatformNode(pool, "platform.audit.view")).Get("/platform/audit", platH.Audit)
			r.With(middleware.RequirePlatformNode(pool, "platform.analytics.view")).Get("/platform/usage", platH.Usage)
			r.With(middleware.RequirePlatformNode(pool, "platform.analytics.view")).Get("/platform/metrics", handler.Metrics(cfg.ValkeyAddr, pool))
			r.With(middleware.RequirePlatformNode(pool, "platform.flags.manage")).Get("/platform/flags", platH.Flags)
			r.With(middleware.RequirePlatformNode(pool, "platform.flags.manage")).Post("/platform/flags/{key}", platH.SetFlag)
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
			r.Get("/me/memberships", memberH.MyMemberships)
			r.Get("/me/finance", finH.MyFinance) // cross-org member view
			r.Get("/me/claims", finH.MyClaims)
			r.Get("/me/calendar", actH.MyCalendar)
			r.Get("/me/action-items", actH.MyItems)
			r.Get("/me/notifications", commsH.MyNotifications)
			r.Post("/me/notifications/{notification_id}/read", commsH.ReadNotification)
			r.Get("/me/preferences", commsH.GetPrefs)
			r.With(middleware.RateLimit(vk, "ws", 30, time.Minute)).Get("/ws", wsGw.ServeWS)
			r.Patch("/me/preferences", commsH.SetPrefs)
			r.Get("/org-templates", orgH.Templates)
			r.Post("/orgs", orgH.Create)
			r.With(middleware.RateLimit(vk, "invite_accept", 10, time.Minute)).Post("/invitations/{token}/accept", orgH.Accept)
			// applicant self-service — no OrgCtx (a pre-decision applicant
			// has no membership); the handler verifies ownership.
			r.Post("/orgs/{org_id}/applications/{application_id}/withdraw", memberH.Withdraw)
			// member self-resign — suspended members carry no OrgCtx but may
			// still exit (§11); the service resolves their membership.
			r.Post("/orgs/{org_id}/resign", memberH.Resign)
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

				// §11 members module — gated on the `members` app being
				// active for this tenant (RequireAppActive seam, §06).
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "members"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view_any")).Get("/orgs/{org_id}/members", memberH.Members)
					r.Get("/orgs/{org_id}/members/{member_id}", memberH.Member)                 // view_any or self — checked inside
					r.Patch("/orgs/{org_id}/members/{member_id}", memberH.PatchMember)          // manage, or self (unit only) — checked inside
					r.Post("/orgs/{org_id}/members/{member_id}/transition", memberH.Transition) // manage — or member self-resigns
					r.Post("/orgs/{org_id}/members/{member_id}/renew", memberH.Renew)           // manage — self path lands with payment (Phase 09)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.card.issue")).Post("/orgs/{org_id}/members/{member_id}/card", memberH.Card)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view_any")).Get("/orgs/{org_id}/members/{member_id}/history", memberH.History)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.manage")).Post("/orgs/{org_id}/applications", memberH.Apply)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.approve")).Get("/orgs/{org_id}/applications", memberH.Applications)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.approve")).Post("/orgs/{org_id}/applications/{application_id}/decision", memberH.Decide)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.import")).Post("/orgs/{org_id}/members/import", memberH.Import)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.export")).Get("/orgs/{org_id}/members/export", memberH.Export)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.export")).Post("/orgs/{org_id}/members/export", memberH.ExportAsync)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.members.export")).Get("/orgs/{org_id}/members/exports/{export_id}", memberH.ExportDownload)
				})

				// §12 finance-lite — verified provenance: every credit is
				// claim→confirm (member) or record→approve (officer,
				// recorder != approver). Ledger is append-only (Rule 19).
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "finance_lite"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.obligations.manage")).Get("/orgs/{org_id}/finance/contribution-types", finH.ContributionTypes)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.obligations.manage")).Post("/orgs/{org_id}/finance/contribution-types", finH.CreateContributionType)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.obligations.manage")).Get("/orgs/{org_id}/finance/schedules", finH.Schedules)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.obligations.manage")).Post("/orgs/{org_id}/finance/schedules", finH.CreateSchedule)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.obligations.manage")).Patch("/orgs/{org_id}/finance/schedules/{schedule_id}", finH.UpdateSchedule)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/schedules/{schedule_id}/status", finH.ScheduleStatus)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/obligations", finH.Obligations)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/obligations/status", finH.ObligationsStatus)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.obligations.manage")).Post("/orgs/{org_id}/finance/obligations/{obligation_id}/waive", finH.WaiveObligation)
					r.With(middleware.RateLimit(vk, "payment", 20, time.Minute)).Post("/orgs/{org_id}/finance/payments", finH.RecordPayment) // view_own or payments.record — inside
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.payments.approve")).Post("/orgs/{org_id}/finance/payments/{payment_id}/confirm", finH.ConfirmPayment)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.payments.approve")).Post("/orgs/{org_id}/finance/payments/{payment_id}/reverse", finH.ReversePayment)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reconcile")).Post("/orgs/{org_id}/finance/payments/{payment_id}/reconcile", finH.ReconcilePayment)
					r.Get("/orgs/{org_id}/finance/receipts/{receipt_id}", finH.Receipt) // view_own or reports.read — service scopes
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/summary", finH.Summary)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/defaulters", finH.Defaulters)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/ledger", finH.Ledger)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/statement", finH.Statement)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/contributions", finH.Contributions)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/activities/{activity_id}/donations", finH.Donations)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/drives", finH.Drives)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.drives.manage")).Post("/orgs/{org_id}/finance/drives", finH.OpenDrive)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/drives/{drive_id}", finH.Drive)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.drives.manage")).Patch("/orgs/{org_id}/finance/drives/{drive_id}", finH.PatchDrive)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.drives.manage")).Post("/orgs/{org_id}/finance/drives/{drive_id}/close", finH.CloseDrive)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.payments.approve")).Post("/orgs/{org_id}/finance/drives/{drive_id}/disburse", finH.DisburseDrive)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.statement_view")).Get("/orgs/{org_id}/finance/drives/{drive_id}/contributions", finH.DriveContributions)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.claims.submit_own"), middleware.RateLimit(vk, "payment", 20, time.Minute)).Post("/orgs/{org_id}/finance/claims", finH.SubmitClaim)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.claims.review")).Get("/orgs/{org_id}/finance/claims", finH.Claims)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.claims.review")).Post("/orgs/{org_id}/finance/claims/{claim_id}/confirm", finH.ConfirmClaim)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.claims.review")).Post("/orgs/{org_id}/finance/claims/{claim_id}/reject", finH.RejectClaim)
				})

				// §13 events & meetings — meetings are governance, events are
				// social (Rule 22); gated on the events_meetings app (§06).
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "events_meetings"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/meetings", actH.Meetings)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.meetings.manage")).Post("/orgs/{org_id}/meetings", actH.CreateMeeting)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/meetings/{activity_id}", actH.Get)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.meetings.manage")).Patch("/orgs/{org_id}/meetings/{activity_id}", actH.PatchMeeting)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.meetings.attendance.record")).Post("/orgs/{org_id}/meetings/{activity_id}/attendance", actH.Attendance)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/meetings/{activity_id}/attendance", actH.Sheet)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.meetings.manage")).Post("/orgs/{org_id}/meetings/{activity_id}/minutes", actH.DraftMinutes)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/meetings/{activity_id}/minutes", actH.Minutes)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.minutes.approve")).Post("/orgs/{org_id}/meetings/{activity_id}/minutes/approve", actH.ApproveMinutes)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/meetings/{activity_id}/agenda", actH.Agenda)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/meetings/{activity_id}/action-items", actH.Items)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.action_items.manage")).Post("/orgs/{org_id}/meetings/{activity_id}/action-items", actH.CreateItem)
					r.Patch("/orgs/{org_id}/meetings/{activity_id}/action-items/{item_id}", actH.PatchItem) // manage or self — checked inside
					r.With(middleware.RequireOrgPermission(orgSvc, "org.meetings.manage")).Post("/orgs/{org_id}/meetings/{activity_id}/quorum", actH.Quorum)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/events", actH.Events)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.events.manage")).Post("/orgs/{org_id}/events", actH.CreateEvent)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/events/{activity_id}", actH.Get)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.events.manage")).Patch("/orgs/{org_id}/events/{activity_id}", actH.PatchEvent)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.events.register_own")).Post("/orgs/{org_id}/events/{activity_id}/register", actH.Register)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.events.manage")).Post("/orgs/{org_id}/events/{activity_id}/checkin", actH.CheckIn)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/events/{activity_id}/registrations", actH.Registrations)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/calendar", actH.Calendar)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.activities.view")).Get("/orgs/{org_id}/recurrences", actH.Recurrences)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.meetings.manage")).Post("/orgs/{org_id}/recurrences", actH.CreateRecurrence)
				})

				// §14 comms & documents — announcements gated on the app.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "comms_documents"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.view")).Get("/orgs/{org_id}/announcements", commsH.Announcements)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.announce")).Post("/orgs/{org_id}/announcements", commsH.CreateAnnouncement)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.announce")).Post("/orgs/{org_id}/announcements/{announcement_id}/publish", commsH.PublishAnnouncement)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.announce")).Post("/orgs/{org_id}/announcements/{announcement_id}/retract", commsH.RetractAnnouncement)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.documents.upload"), middleware.RateLimit(vk, "upload", 10, time.Minute)).Post("/orgs/{org_id}/documents", commsH.UploadDocument)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.documents.view")).Get("/orgs/{org_id}/documents", commsH.Documents)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.documents.view")).Get("/orgs/{org_id}/documents/{document_id}/download", commsH.DownloadDocument)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.documents.publish")).Patch("/orgs/{org_id}/documents/{document_id}", commsH.PatchDocument)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.documents.archive")).Delete("/orgs/{org_id}/documents/{document_id}", commsH.ArchiveDocument)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.documents.upload")).Post("/orgs/{org_id}/documents/{document_id}/versions", commsH.NewVersion)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.documents.view")).Get("/orgs/{org_id}/documents/{document_id}/versions", commsH.DocumentVersions)
					// §24 chat — same comms_documents app group.
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Get("/orgs/{org_id}/channels", chatH.Channels)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.channels.manage")).Post("/orgs/{org_id}/channels", chatH.CreateChannel)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.channels.manage")).Post("/orgs/{org_id}/channels/{channel_id}/members", chatH.AddMember)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.channels.manage")).Post("/orgs/{org_id}/channels/{channel_id}/archive", chatH.Archive)
					r.Delete("/orgs/{org_id}/channels/{channel_id}/members/{member_id}", chatH.RemoveMember) // manage or self — inside
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Get("/orgs/{org_id}/channels/{channel_id}/messages", chatH.Messages)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.post"), middleware.RateLimit(vk, "chat", 60, time.Minute)).Post("/orgs/{org_id}/channels/{channel_id}/messages", chatH.PostMessage)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Patch("/orgs/{org_id}/channels/{channel_id}/messages/{message_id}", chatH.EditMessage)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Delete("/orgs/{org_id}/channels/{channel_id}/messages/{message_id}", chatH.DeleteMessage)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Post("/orgs/{org_id}/channels/{channel_id}/read", chatH.MarkRead)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Post("/orgs/{org_id}/channels/{channel_id}/mute", chatH.Mute)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Get("/orgs/{org_id}/voice-rooms", chatH.LiveRooms)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.voice.start")).Post("/orgs/{org_id}/voice-rooms", chatH.StartRoom)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.voice.join")).Post("/orgs/{org_id}/voice-rooms/{room_id}/join", chatH.JoinRoom)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.voice.join")).Post("/orgs/{org_id}/voice-rooms/{room_id}/leave", chatH.LeaveRoom)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Post("/orgs/{org_id}/voice-rooms/{room_id}/end", chatH.EndRoom)
				})
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
