// Ref: oga9-engineering/sections/01-foundation-tech-stack.md
// Ref: oga9-engineering/sections/08-job-queue-asynq.md
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
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
	"github.com/jedafe/oga9/internal/ai"
	"github.com/jedafe/oga9/internal/analytics"
	"github.com/jedafe/oga9/internal/assets"
	"github.com/jedafe/oga9/internal/auth"
	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/blog"
	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/comms"
	"github.com/jedafe/oga9/internal/config"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/elections"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/finance"
	"github.com/jedafe/oga9/internal/governance"
	"github.com/jedafe/oga9/internal/handler"
	"github.com/jedafe/oga9/internal/integrations"
	"github.com/jedafe/oga9/internal/license"
	"github.com/jedafe/oga9/internal/mailer"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/obs"
	"github.com/jedafe/oga9/internal/otp"
	"github.com/jedafe/oga9/internal/platform"
	"github.com/jedafe/oga9/internal/projects"
	"github.com/jedafe/oga9/internal/realtime"
	"github.com/jedafe/oga9/internal/search"
	"github.com/jedafe/oga9/internal/secrets"
	"github.com/jedafe/oga9/internal/storage"
	"github.com/jedafe/oga9/internal/tasks"
	"github.com/jedafe/oga9/internal/tenant"
	"github.com/jedafe/oga9/internal/welfare"
)

// egressClient serves admin-configured outbound targets (connector
// locator_url pushes): a hard timeout plus a per-dial public-IP check so
// DNS resolving to loopback/private/link-local space can't reach it.
var egressClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, err
			}
			d := net.Dialer{Timeout: 10 * time.Second}
			for _, a := range ips {
				ip := a.IP
				if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
					ip.IsLinkLocalMulticast() || ip.IsMulticast() || !ip.IsGlobalUnicast() {
					continue
				}
				return d.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			}
			return nil, fmt.Errorf("egress: no public address for %s", host)
		},
	},
}

// smsClient — the operator-configured SMS gateway gets a timeout but no
// IP restriction: self-hosted gateways legitimately live on private nets.
var smsClient = &http.Client{Timeout: 15 * time.Second}

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
	// Browser-direct API calls (SvelteKit PWA on app.{base} subdomains):
	// allowlisted-origin CORS — Bearer auth means no credential sharing.
	r.Use(middleware.CORS(middleware.CORSOrigins(cfg.BaseDomain, cfg.AppURL, cfg.Env != "production")))
	// §13.05: structured access log + per-route latency reservoir.
	r.Use(obs.Metrics)
	r.Use(obs.RequestLog)
	// §13.03: global read + write tiers — bound per-IP traffic; stricter
	// per-endpoint buckets (auth/upload/payment/platform) stack on top.
	r.Use(middleware.RateLimitReads(vk, "read", 240, time.Minute))
	r.Use(middleware.RateLimitWrites(vk, "write", 120, time.Minute))

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
	orgSvc.SetBaseDomain(cfg.BaseDomain) // §04 custom_domain hijack guard
	finSvc := finance.NewService(pool, vk)
	// §07: limit.members.active gate at membership activation (Rule 25).
	orgSvc.SetEntitlements(entSvc)
	// §10 phone-targeted invites: OTP proof of the invited number before a
	// bearer link mints a membership. Delivery is a generic HTTPS POST
	// gateway — unconfigured leaves phone invites unverifiable and the
	// accept path fails closed (never silently degrades to bearer).
	otpSvc := otp.NewService(vk)
	switch {
	case cfg.WhatsAppPhoneNumberID != "" && cfg.WhatsAppToken != "" && cfg.WhatsAppTemplate != "":
		// Meta Cloud API preferred — authentication template carries the code.
		otpSvc.SetSender(otp.WhatsAppSender(cfg.WhatsAppPhoneNumberID, cfg.WhatsAppToken,
			cfg.WhatsAppTemplate, cfg.WhatsAppLang, cfg.WhatsAppOTPButton))
	case cfg.SMSGatewayURL != "":
		otpSvc.SetSender(func(ctx context.Context, destination, code string) error {
			payload, _ := json.Marshal(map[string]string{
				"to":   destination,
				"text": "Your OGA9 verification code is " + code,
			})
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.SMSGatewayURL, bytes.NewReader(payload))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			if cfg.SMSGatewayKey != "" {
				req.Header.Set("Authorization", "Bearer "+cfg.SMSGatewayKey)
			}
			resp, err := smsClient.Do(req)
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			if resp.StatusCode >= 300 {
				return fmt.Errorf("sms gateway: %d", resp.StatusCode)
			}
			return nil
		})
	}
	orgSvc.SetInviteOTP(otpSvc)
	middleware.SetRateLimitStrict(cfg.RateLimitStrict)
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
	commsSvc.SetValkey(vk)
	// §16 governance — motions/resolutions/constitution/mandates/handover.
	// Seams keep module boundaries (Rule 26): quorum checks go through
	// activities' service, handover archives through documents.
	govSvc := governance.NewService(pool)
	// §16 discipline 'suspension' sanction lands as a real lifecycle
	// transition through the members seam (Rule 26).
	govSvc.SetSuspension(func(ctx context.Context, oc *tenant.OrgCtx, memberID uuid.UUID) error {
		return orgSvc.TransitionMember(ctx, oc, memberID, "suspend")
	})
	govSvc.SetQuorum(func(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) (bool, error) {
		met, _, err := actSvc.EvaluateQuorum(ctx, oc, aid)
		return met, err
	})
	govSvc.SetArchive(func(ctx context.Context, oc *tenant.OrgCtx, title string, r io.Reader) error {
		_, err := commsSvc.Upload(ctx, oc, "archive", title, "exco", uuid.Nil, r)
		return err
	})
	// §16 fine sanctions post through the §12 fine seam — never off-ledger.
	govSvc.SetFine(finSvc.AssessFine)
	// §17 welfare — cases/approvals/disbursement/visits.
	welSvc := welfare.NewService(pool)
	welH := handler.NewWelfareHandler(welSvc)
	// §19 analytics — async snapshots + versioned health + report runs.
	// Seams keep module boundaries: report artifacts go through documents,
	// consolidation gates on capability.consolidation (business tier).
	anaSvc := analytics.NewService(pool)
	anaSvc.SetArchive(func(ctx context.Context, oc *tenant.OrgCtx, title string, r io.Reader) (uuid.UUID, error) {
		return commsSvc.Upload(ctx, oc, "report", title, "exco", uuid.Nil, r)
	})
	anaSvc.SetCapability(func(ctx context.Context, tid uuid.UUID, cap string) bool {
		return entSvc.IsEnabled(ctx, tid, cap)
	})
	anaSvc.SetMe(func(ctx context.Context, uid uuid.UUID) (any, error) {
		return orgSvc.MyMemberships(ctx, uid)
	})
	anaSvc.SetPermissions(func(ctx context.Context, uid, oid uuid.UUID, node string) bool {
		return orgSvc.HasPermission(ctx, uid, oid, node)
	})
	anaSvc.SetDocArtifact(func(ctx context.Context, oc *tenant.OrgCtx, docID uuid.UUID) (io.ReadCloser, string, error) {
		rc, doc, err := commsSvc.Download(ctx, oc, docID, false)
		if err != nil {
			return nil, "", err
		}
		return rc, doc.Title, nil
	})
	// §19 consolidated: per-branch roll-up — members + activity counts for
	// every org_units row of type 'branch'.
	anaSvc.SetBranches(func(ctx context.Context, oc *tenant.OrgCtx) ([]map[string]any, error) {
		units, err := orgSvc.ListUnits(ctx, oc.OrgID)
		if err != nil {
			return nil, err
		}
		var out []map[string]any
		for _, u := range units {
			if u.Type != "branch" {
				continue
			}
			uid := u.ID
			ms, _ := orgSvc.Members(ctx, oc, "", "", uid)
			acts, _ := actSvc.List(ctx, oc, "", "", uid)
			out = append(out, map[string]any{
				"unit_id": uid, "name": u.Name,
				"members": len(ms), "activities": len(acts)})
		}
		return out, nil
	})
	// §19 dashboard sections + health components — real reads from the
	// owning modules' services (Rule 26), not fabricated zeros.
	anaSvc.SetSection("members", func(ctx context.Context, oc *tenant.OrgCtx, role string) (any, error) {
		return orgSvc.Members(ctx, oc, "", "", uuid.Nil)
	})
	anaSvc.SetSection("activities", func(ctx context.Context, oc *tenant.OrgCtx, role string) (any, error) {
		return actSvc.List(ctx, oc, "", "", uuid.Nil)
	})
	anaSvc.SetSection("finance", func(ctx context.Context, oc *tenant.OrgCtx, _ string) (any, error) {
		// §19: content filtered by the viewer's permission NODES (Rule 20),
		// not position names — any holder of reports.read sees the section.
		if !orgSvc.HasPermission(ctx, oc.UserID, oc.OrgID, "org.finance.reports.read") {
			return nil, nil
		}
		return finSvc.Statement(ctx, oc)
	})
	anaSvc.SetSection("welfare", func(ctx context.Context, oc *tenant.OrgCtx, _ string) (any, error) {
		// welfare cases are org.welfare.view_any data — a member without
		// the node's snapshot must not embed them (§19: permission nodes).
		if !orgSvc.HasPermission(ctx, oc.UserID, oc.OrgID, "org.welfare.view_any") {
			return nil, nil
		}
		return welSvc.ListCases(ctx, oc, "")
	})
	// health.* producers: 0-100 component scores from real org signals.
	anaSvc.SetSection("health.membership", func(ctx context.Context, oc *tenant.OrgCtx, _ string) (any, error) {
		all, err := orgSvc.Members(ctx, oc, "", "", uuid.Nil)
		if err != nil || len(all) == 0 {
			return int64(0), err
		}
		var active int64
		for _, m := range all {
			if m.LifecycleState == "active" {
				active++
			}
		}
		return int64(active * 100 / int64(len(all))), nil
	})
	anaSvc.SetSection("health.attendance", func(ctx context.Context, oc *tenant.OrgCtx, _ string) (any, error) {
		// proxy: share of recent meetings that actually happened
		// (held/closed vs cancelled) in the activity list.
		list, err := actSvc.List(ctx, oc, "meeting", "", uuid.Nil)
		if err != nil || len(list) == 0 {
			return int64(50), err // no meetings → neutral, not zero
		}
		var held, total int64
		for _, a := range list {
			if a.Status == "cancelled" {
				continue
			}
			total++
			if a.Status == "completed" || a.Status == "held" || a.Status == "closed" {
				held++
			}
		}
		if total == 0 {
			return int64(50), nil
		}
		return int64(held * 100 / total), nil
	})
	anaSvc.SetSection("health.finance", func(ctx context.Context, oc *tenant.OrgCtx, _ string) (any, error) {
		all, err := orgSvc.Members(ctx, oc, "", "", uuid.Nil)
		if err != nil {
			return int64(0), err
		}
		def, err := finSvc.Defaulters(ctx, oc)
		if err != nil {
			return int64(0), err
		}
		if len(all) == 0 {
			return int64(100), nil
		}
		good := int64(len(all) - len(def))
		if good < 0 {
			good = 0
		}
		return int64(good * 100 / int64(len(all))), nil
	})
	anaSvc.SetSection("health.engagement", func(ctx context.Context, oc *tenant.OrgCtx, _ string) (any, error) {
		// recent domain-event volume as the engagement proxy — scaled
		// 0-100 over a modest baseline (10 events/mo = fully engaged).
		act, err := actSvc.List(ctx, oc, "", "", uuid.Nil)
		if err != nil {
			return int64(0), err
		}
		n := int64(len(act) + 1)
		if n > 10 {
			n = 10
		}
		return int64(n * 10), nil
	})
	anaH := handler.NewAnalyticsHandler(anaSvc)
	// §18 assets — registry, term-scoped custody, audits, credential refs.
	astSvc := assets.NewService(pool)
	astH := handler.NewAssetsHandler(astSvc)
	// §16 handover report sections — each produced by the owning module's
	// service (Rule 26): open action items, open custody, pending
	// approvals, finances snapshot.
	govSvc.SetHandoverSection("action_items", func(ctx context.Context, oc *tenant.OrgCtx) (any, error) {
		return actSvc.OpenActionItemsForOrg(ctx, oc)
	})
	govSvc.SetHandoverSection("custody", func(ctx context.Context, oc *tenant.OrgCtx) (any, error) {
		return astSvc.OpenCustody(ctx, oc)
	})
	govSvc.SetHandoverSection("pending_approvals", func(ctx context.Context, oc *tenant.OrgCtx) (any, error) {
		cases, err := welSvc.ListCases(ctx, oc, "requested")
		if err != nil {
			return nil, err
		}
		reviewing, err := welSvc.ListCases(ctx, oc, "reviewing")
		if err != nil {
			return nil, err
		}
		claims, err := finSvc.PendingClaims(ctx, oc)
		if err != nil {
			return nil, err
		}
		return map[string]any{"welfare_requested": cases, "welfare_reviewing": reviewing, "payment_claims": claims}, nil
	})
	govSvc.SetHandoverSection("finance", func(ctx context.Context, oc *tenant.OrgCtx) (any, error) {
		return finSvc.Statement(ctx, oc)
	})

	// §20 projects — resolution-gated delivery, completion reports via
	// the documents seam.
	prjSvc := projects.NewService(pool)
	prjSvc.SetArchive(func(ctx context.Context, oc *tenant.OrgCtx, title string, r io.Reader) (uuid.UUID, error) {
		return commsSvc.Upload(ctx, oc, "report", title, "committee", uuid.Nil, r)
	})
	// §16+§20: mandate enforcement via the governance seam; spend totals
	// come from project_expenses reservations (same-tx, race-free caps).
	prjSvc.SetSpendGate(govSvc.MandateLimit)
	finSvc.SetMandateLimit(govSvc.MandateLimit) // §25 committee-capped expenses
	prjSvc.SetPaymentVerifier(func(ctx context.Context, orgID, pid uuid.UUID) (bool, error) {
		return finSvc.PaymentExists(ctx, orgID, pid)
	})
	prjH := handler.NewProjectsHandler(prjSvc)
	// §21 elections — nominations, secret ballots (participation vs choice),
	// ratification seats winners via the governance appointment seam.
	elcSvc := elections.NewService(pool)
	elcSvc.SetAppoint(func(ctx context.Context, oc *tenant.OrgCtx, termID, positionID, memberID, branchID, committeeID uuid.UUID, startsAt time.Time) (uuid.UUID, error) {
		return orgSvc.AppointOfficer(ctx, oc, termID, positionID, memberID, branchID, committeeID, startsAt)
	})
	elcH := handler.NewElectionsHandler(elcSvc)
	// §23 integrations — connectors, signed webhooks, api keys, japa sync.
	// Secrets resolve via credential_ref → env lookup in dev; the locator
	// join lands as a membership application via the tenant seam.
	intSvc := integrations.NewService(pool)
	// §23 oga9:ics:{org_id} feed cache (Rule 4 catalogued key).
	intSvc.SetFeedCache(
		func(ctx context.Context, orgID uuid.UUID) (string, error) {
			b, err := vk.Do(ctx, vk.B().Get().Key("oga9:ics:"+orgID.String()).Build()).AsBytes()
			return string(b), err
		},
		func(ctx context.Context, orgID uuid.UUID, feed string) error {
			return vk.Do(ctx, vk.B().Set().Key("oga9:ics:"+orgID.String()).Value(feed).Ex(time.Hour).Build()).Error()
		})
	// credential_ref resolves through the secrets seam — env: and file:
	// schemes only; raw secrets are never stored (§23 rule). file: refs
	// are confined to OGA9_SECRETS_DIR; env: refs never read OGA9_* names.
	secrets.SetFileRoot(cfg.SecretsDir)
	intSvc.SetSecret(secrets.Resolve)
	intSvc.SetApply(orgSvc.PublicApply)
	// §23 ICS feed — renders the org's activities as VCALENDAR.
	intSvc.SetCalendar(func(ctx context.Context, oc *tenant.OrgCtx) (string, error) {
		acts, err := actSvc.OrgCalendar(ctx, oc, uuid.Nil)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		b.WriteString("BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//OGA9//EN\r\n")
		for _, a := range acts {
			b.WriteString("BEGIN:VEVENT\r\nUID:" + uuid.UUID(a.ID.Bytes).String() + "\r\n")
			b.WriteString("DTSTART:" + a.StartsAt.Time.UTC().Format("20060102T150405Z") + "\r\n")
			if a.EndsAt.Valid {
				b.WriteString("DTEND:" + a.EndsAt.Time.UTC().Format("20060102T150405Z") + "\r\n")
			}
			b.WriteString("SUMMARY:" + strings.NewReplacer("\r\n", " ", "\n", " ", ";", "\\;", ",", "\\,").Replace(a.Title) + "\r\n")
			b.WriteString("END:VEVENT\r\n")
		}
		b.WriteString("END:VCALENDAR\r\n")
		return b.String(), nil
	})
	// outbound sync transport — POST job payload to the connector's
	// locator_url (ADR-021); credential_ref supplies the bearer token.
	// The private-target opt-in swaps in a plain client (self-hosted,
	// in-house connectors); default keeps the public-IP egress guard.
	pushClient := egressClient
	if cfg.ConnectorAllowPrivateTargets {
		pushClient = smsClient
	}
	intSvc.SetAllowPrivateTargets(cfg.ConnectorAllowPrivateTargets)
	intSvc.SetPost(func(ctx context.Context, url, auth string, payload []byte) error {
		req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := pushClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("locator ingestion %d", resp.StatusCode)
		}
		return nil
	})
	intH := handler.NewIntegrationsHandler(intSvc, queue, cfg.BaseDomain)
	// §22 ai — provider/apply/limit seams; permission-scoped answers,
	// HITL drafts, limit.ai.requests metering. The OpenAI-compatible
	// adapter wires only when configured — unconfigured → 503
	// ai_unavailable (honest degradation, no fake answers).
	aiSvc := ai.NewService(pool)
	if cfg.AIProviderURL != "" {
		adapter := ai.NewOpenAICompat(cfg.AIProviderURL, cfg.AIProviderKey, cfg.AIProviderModel)
		aiSvc.SetProvider(adapter.Chat)
		aiSvc.SetDraftProvider(adapter.Draft)
	}
	aiSvc.SetLimit(func(ctx context.Context, tenantID uuid.UUID, key string) int64 {
		return entSvc.Limit(ctx, tenantID, key)
	})
	aiSvc.SetPermissions(func(ctx context.Context, uid, oid uuid.UUID, node string) bool {
		return orgSvc.HasPermission(ctx, uid, oid, node)
	})
	// feature_flags wins (the /platform/flags surface writes there —
	// e.g. platform.ai_assistant.enabled); system_settings is the fallback.
	aiSvc.SetFlag(func(ctx context.Context, key string) bool {
		q := db.New(pool)
		if enabled, err := q.GetFeatureFlag(ctx, key); err == nil {
			return enabled
		}
		v, err := q.GetSystemSetting(ctx, key)
		if err != nil {
			return false // unset → disabled; post-MVP flags ship off (flag convention)
		}
		var b bool
		if json.Unmarshal(v, &b) == nil {
			return b
		}
		return false
	})
	aiSvc.SetCostRate(cfg.AICostPer1KMicroUSD)
	// §22 HITL apply: approved drafts land in their domain through the
	// owning services (Rule 26) — AI never writes domain tables.
	aiSvc.SetApply(func(ctx context.Context, oc *tenant.OrgCtx, g db.AiGenerations) error {
		var d struct {
			Title    string `json:"title"`
			Body     string `json:"body"`
			TargetID string `json:"target_id"`
		}
		_ = json.Unmarshal(g.Draft, &d)
		tid, _ := uuid.Parse(d.TargetID)
		switch g.Kind {
		case "agenda":
			if tid == uuid.Nil {
				return fmt.Errorf("agenda draft needs a target meeting")
			}
			items, _ := json.Marshal([]map[string]any{{"order": 1, "title": d.Title, "body": d.Body}})
			return actSvc.SetAgenda(ctx, oc, tid, items)
		case "minutes":
			if tid == uuid.Nil {
				return fmt.Errorf("minutes draft needs a target meeting")
			}
			_, err := actSvc.DraftMinutes(ctx, oc, tid, d.Body)
			return err
		case "announcement":
			_, err := commsSvc.Publish(ctx, oc, d.Title, d.Body, comms.Audience{All: true}, true)
			return err
		case "report", "insight":
			_, err := commsSvc.Upload(ctx, oc, "report", "ai-"+g.Kind+"-"+g.ID.String()[:8],
				"exco", uuid.Nil, strings.NewReader(d.Body))
			return err
		}
		return nil
	})
	// §22 context assembly: permission-scoped grounding sources — the
	// asker only ever sees entities their nodes cover.
	aiSvc.AddContextSource("org.governance.view", func(ctx context.Context, oc *tenant.OrgCtx) (string, []ai.Citation, error) {
		ms, err := govSvc.ListMotions(ctx, oc, "")
		if err != nil || len(ms) == 0 {
			return "", nil, err
		}
		var b strings.Builder
		var cs []ai.Citation
		for i, m := range ms {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "motion %s: %s (%s)\n", m.ID, m.Title, m.State)
			cs = append(cs, ai.Citation{Type: "motion", ID: uuid.UUID(m.ID.Bytes), Label: m.Title})
		}
		return b.String(), cs, nil
	})
	aiSvc.AddContextSource("org.finance.reports.read", func(ctx context.Context, oc *tenant.OrgCtx) (string, []ai.Citation, error) {
		st, err := finSvc.Statement(ctx, oc)
		if err != nil {
			return "", nil, err
		}
		raw, _ := json.Marshal(st)
		return "finance statement: " + string(raw), nil, nil
	})
	aiSvc.AddContextSource("org.activities.view", func(ctx context.Context, oc *tenant.OrgCtx) (string, []ai.Citation, error) {
		list, err := actSvc.List(ctx, oc, "", "", uuid.Nil)
		if err != nil || len(list) == 0 {
			return "", nil, err
		}
		var b strings.Builder
		var cs []ai.Citation
		for i, a := range list {
			if i >= 10 {
				break
			}
			fmt.Fprintf(&b, "activity %s: %s (%s, %s)\n", a.ID, a.Title, a.Kind, a.Status)
			cs = append(cs, ai.Citation{Type: "activity", ID: uuid.UUID(a.ID.Bytes), Label: a.Title})
		}
		return b.String(), cs, nil
	})
	aiSvc.AddContextSource("org.members.view", func(ctx context.Context, oc *tenant.OrgCtx) (string, []ai.Citation, error) {
		ms, err := orgSvc.Members(ctx, oc, "", "", uuid.Nil)
		if err != nil {
			return "", nil, err
		}
		return fmt.Sprintf("member count: %d", len(ms)), nil, nil
	})
	aiH := handler.NewAIHandler(aiSvc, queue)
	// §14/ADR-019: S3-compatible blob backend (SeaweedFS) when configured;
	// local disk stays the default so self-hosted never needs it (Rule 29).
	if cfg.S3Endpoint != "" {
		commsSvc.SetStore(storage.NewS3(cfg.S3Endpoint, cfg.S3Bucket, cfg.S3AccessKey, cfg.S3SecretKey).WithImgproxy(cfg.ImgproxyURL))
	} else {
		commsSvc.SetStore(storage.New(cfg.UploadsDir))
	}
	commsSvc.SetLimitFn(entSvc.Limit)
	commsSvc.SetUploadFallbackMB(cfg.UploadMaxMB)
	// Rule-26 read seams — comms never reads activities/finance tables.
	commsSvc.SetActivityInfo(actSvc.ActivitySnapshot)
	commsSvc.SetPaymentInfo(finSvc.PaymentSnapshot)
	commsH := handler.NewCommsHandler(commsSvc, queue, orgSvc)
	govH := handler.NewGovernanceHandler(govSvc)
	chatSvc := chat.NewService(pool, vk)
	chatSvc.SetDocGate(commsSvc.DocInOrg)
	chatSvc.SetActivityGate(actSvc.ActivityInOrg)
	chatSvc.SetVoiceProvider(cfg.LiveKitURL, cfg.LiveKitAPIKey, cfg.LiveKitAPISecret, cfg.VoiceJoinWindowSec)
	chatH := handler.NewChatHandler(chatSvc, orgSvc)
	wsGw := realtime.NewGateway(chatSvc, vk, cfg.BaseDomain, cfg.Env != "production")
	// §15 platform control plane — provision pipeline + operator routes.
	platSvc := platform.NewService(pool)
	platSvc.SetStore(storage.New(cfg.UploadsDir)) // §13.06 async tenant exports
	platSvc.SetExportDataKey(cfg.DataKey)
	platSvc.SetExportSigningKey(cfg.LicenseSignKey) // ADR-026 bundle signature
	// §15 scaffold: the tenant module owns role/app rows — clone templates,
	// install MVP apps, grant the owner org_admin inside the provision tx.
	platSvc.SetTenantScaffolder(tenant.ScaffoldTenant)
	platSvc.SetOrgBootstrap(func(ctx context.Context, userID uuid.UUID, name, slug, templateSlug, orgType, country, city, billingEmail string, requestedTenant uuid.UUID) (uuid.UUID, error) {
		org, err := orgSvc.CreateOrg(ctx, userID, name, slug, templateSlug, orgType, country, city, billingEmail, requestedTenant, entSvc)
		if err != nil {
			return uuid.Nil, err
		}
		return org.ID, nil
	})
	platH := handler.NewPlatformHandler(platSvc, queue)
	texH := handler.NewTenantExportHandler(platSvc, queue, orgSvc.HasPermission)
	taskDeps.Activities = actSvc
	taskDeps.Comms = commsSvc
	taskDeps.Governance = govSvc
	taskDeps.Analytics = anaSvc
	taskDeps.Assets = astSvc
	taskDeps.Elections = elcSvc
	taskDeps.Integrations = intSvc
	taskDeps.AI = aiSvc
	taskDeps.Ent = entSvc
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
	// §14.03/Rule 29: the verified license file feeds entitlement
	// resolution on self-hosted editions — valid and offline-grace files
	// merge their signed {capabilities,limits}; expired/invalid/community
	// resolve to the plan floor (the §09 degrade path is now real, not
	// just a log line). The file is re-read on cache miss, so a replaced
	// file takes effect within the 10min entitlement cache TTL.
	entSvc.SetLicenseSource(func(_ context.Context) (json.RawMessage, bool) {
		st, f, err := licV.Check()
		if err != nil || f == nil || (st != license.StatusValid && st != license.StatusGrace) {
			return nil, false
		}
		return f.Entitlements, true
	})
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
	// WhatsApp inbound — free phone-verification channel (service
	// conversation): member texts "JOIN <code>" to the WABA number.
	orgH.SetWhatsAppWebhook(cfg.WhatsAppVerifyToken, cfg.WhatsAppAppSecret)
	orgSvc.SetWhatsAppInbound(cfg.WhatsAppVerifyToken != "" && cfg.WhatsAppAppSecret != "",
		cfg.WhatsAppDisplayNumber)
	fieldsH := handler.NewFieldsHandler(orgSvc)
	blogH := handler.NewBlogHandler(blog.NewService(pool), "https://"+cfg.BaseDomain) // platform blog lives on the marketing surface
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
	// §23 japa sync — outbound push on publish/location events (ADR-021).
	dispatcher.Register("japa_sync", intSvc.JapaPushConsumer)
	// §14: member-targeted events (overdue items, dues flags) → in_app
	// notifications gated on preferences.
	dispatcher.Register("member_notify", commsSvc.EventNotifyConsumer)
	// §15: subscription/provision changes recompute countable usage meters.
	dispatcher.Register("usage_recompute", billSvc.UsageRecomputeConsumer)
	// §15: tenant status transitions write through to the oga9:tenant_status
	// cache so suspension takes effect without waiting out the 60s TTL.
	dispatcher.Register("tenant_status_cache", orgSvc.TenantStatusCacheConsumer)
	// §17: MemberDeceased → bereavement case suggestion; BenefitDisbursed →
	// finance posts the debit ledger entry (Rule 12/19 outbox pattern).
	dispatcher.Register("welfare_member_deceased", welSvc.MemberDeceasedConsumer)
	dispatcher.Register("welfare_disbursement", finSvc.WelfareDisbursementConsumer)
	// §16: term close (manual or sweep) generates the handover archive —
	// deduped per term alongside the 14-day reminders window.
	dispatcher.Register("governance_handover", govSvc.TermEndHandoverConsumer)
	// §18: term close flags unreturned custody to the incoming EXCO;
	// disposal sales post a credit ledger entry.
	dispatcher.Register("assets_term_ended", astSvc.TermEndedConsumer)
	dispatcher.Register("assets_disposal", finSvc.AssetDisposalConsumer)
	dispatcher.Register("project_expense", finSvc.ProjectExpenseConsumer)
	// ADR-019: search consumer (documents indexing); §27 volunteer cascade.
	if searchSvc := search.New(cfg.MeilisearchURL, cfg.MeilisearchKey); searchSvc != nil {
		if err := searchSvc.EnsureIndex(ctx); err != nil {
			log.Printf("meilisearch index: %v", err)
		}
		dispatcher.Register("search", searchSvc.Consumer)
	}
	dispatcher.Register("volunteer_cancel", actSvc.ActivityCancelledConsumer)
	dispatcher.Register("poll_survey_notify", commsSvc.PollSurveyNotifyConsumer)
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
	// §16 governance sweeps — daily, default tier per the task catalog.
	for _, typ := range []string{tasks.TypeTermTransitions, tasks.TypeHandoverReminders} {
		if _, err := scheduler.Register(
			"@daily",
			asynq.NewTask(typ, nil, asynq.Queue("default")),
		); err != nil {
			log.Fatalf("schedule %s: %v", typ, err)
		}
	}
	// §19 analytics sweeps — low tier: snapshots every 15min; health score
	// + monthly report on the first of the month; projection_rebuild is
	// on-demand only (repair), not scheduled.
	if _, err := scheduler.Register(
		"@every 15m",
		asynq.NewTask(tasks.TypeRefreshSnapshots, nil, asynq.Queue("low")),
	); err != nil {
		log.Fatalf("schedule %s: %v", tasks.TypeRefreshSnapshots, err)
	}
	for _, spec := range []struct{ typ, cron string }{
		{tasks.TypeHealthScore, "0 2 1 * *"},    // 02:00 on the 1st
		{tasks.TypeMonthlyReport, "0 3 1 * *"},  // 03:00 on the 1st
		{tasks.TypeMaintenanceDue, "0 6 * * 1"}, // §18: Mondays 06:00
	} {
		if _, err := scheduler.Register(
			spec.cron,
			asynq.NewTask(spec.typ, nil, asynq.Queue("low")),
		); err != nil {
			log.Fatalf("schedule %s: %v", spec.typ, err)
		}
	}
	// §18: assets:handover_sweep is critical-tier + event-driven (TermEnded
	// consumer); the task type stays registered for manual term redrives.
	// §21 elections — default-tier sweeps: window opens/closes every 15min,
	// reminders hourly.
	for _, spec := range []struct{ typ, cron string }{
		{tasks.TypeElectionsOpenVoting, "@every 15m"},
		{tasks.TypeElectionsCloseVoting, "@every 15m"},
		{tasks.TypeElectionsReminders, "@every 1h"},
		{tasks.TypeJapaSyncPush, "@every 15m"},
	} {
		if _, err := scheduler.Register(
			spec.cron,
			asynq.NewTask(spec.typ, nil, asynq.Queue("default")),
		); err != nil {
			log.Fatalf("schedule %s: %v", spec.typ, err)
		}
	}
	// §24 realtime sweeps — presence (default), room_orphans (default),
	// retention (low).
	for _, spec := range []struct{ typ, cron, queue string }{
		{tasks.TypePresenceSweep, "@every 5m", "default"},
		{tasks.TypeRoomSweep, "@every 15m", "default"},
		{tasks.TypeRetentionSweep, "@daily", "low"},
	} {
		if _, err := scheduler.Register(
			spec.cron,
			asynq.NewTask(spec.typ, nil, asynq.Queue(spec.queue)),
		); err != nil {
			log.Fatalf("schedule %s: %v", spec.typ, err)
		}
	}
	// §23 remaining tiers — webhook processing is critical-tier; the
	// reconcile + ics sweeps are low-tier (task catalog).
	for _, spec := range []struct{ typ, cron, queue string }{
		{tasks.TypeWebhookProcess, "@every 1m", "critical"},
		{tasks.TypeJapaSyncReconcile, "0 4 * * *", "low"},
		{tasks.TypeIcsFeed, "@daily", "low"},
		{tasks.TypeAIUsageRollup, "@daily", "low"},
	} {
		if _, err := scheduler.Register(
			spec.cron,
			asynq.NewTask(spec.typ, nil, asynq.Queue(spec.queue)),
		); err != nil {
			log.Fatalf("schedule %s: %v", spec.typ, err)
		}
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
		r.With(middleware.RateLimit(vk, "directory", 60, time.Minute)).Get("/directory/orgs/{slug}/projects", prjH.PublicProjects)
		// §23: org profile detail + locator join (JapaConnect intake),
		// plus provider webhook endpoints (verify→persist→200, Rule 9).
		r.With(middleware.RateLimit(vk, "directory", 60, time.Minute)).Get("/directory/orgs/{slug}", intH.OrgProfile)
		r.With(middleware.RateLimit(vk, "directory", 60, time.Minute)).Get("/directory/resolve", intH.ResolveHost)
		// §28 platform blog — public published reads + RSS
		r.With(middleware.RateLimit(vk, "directory", 60, time.Minute)).Get("/blog/posts", blogH.Posts)
		r.With(middleware.RateLimit(vk, "directory", 60, time.Minute)).Get("/blog/posts/{slug}", blogH.Post)
		r.With(middleware.RateLimit(vk, "directory", 30, time.Minute)).Get("/blog/rss.xml", blogH.RSS)
		r.With(middleware.RateLimit(vk, "directory_apply", 10, time.Minute)).Post("/directory/orgs/{slug}/join", intH.LocatorJoin)
		r.With(middleware.RateLimit(vk, "webhook", 60, time.Minute)).Post("/webhooks/{provider}", intH.Webhook)
		// WhatsApp inbound — free phone verification: member texts "JOIN <code>"
		// to the WABA number; Meta's webhook verifies the sender's number.
		r.Get("/webhooks/whatsapp", orgH.WhatsAppVerify)
		r.With(middleware.RateLimit(vk, "whatsapp", 120, time.Minute)).Post("/webhooks/whatsapp", orgH.WhatsAppWebhook)

		// §07 self-hosted callback + provider webhook — no session auth;
		// the signed material / HMAC signature is the credential (Rules 9/29).
		r.With(middleware.RateLimit(vk, "license", 30, time.Minute)).Post("/license/validate", billH.ValidateLicense)
		r.With(middleware.RateLimit(vk, "webhook", 300, time.Minute)).Post("/webhooks/billing/{provider}", billH.Webhook)

		// §24 WS upgrade — outside the RequireAuth group: browser upgrades
		// carry no headers, so the ticket-aware middleware resolves Bearer
		// OR ?ticket= (single-use, minted by POST /ws/ticket above).
		r.With(middleware.APIKeyAuth(intSvc),
			middleware.RateLimit(vk, "ws", 30, time.Minute),
			middleware.RequireAuthOrWSTicket(authSvc, vk, cfg.JWTSecret)).Get("/ws", wsGw.ServeWS)

		// authed — session JWT or mfa_token both reach the mfa enrollment routes
		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireAuthOrMFAGate(authSvc, cfg.JWTSecret))
			r.Post("/auth/mfa/setup", mfaH.Setup)
			r.Post("/auth/mfa/confirm", mfaH.Confirm)
		})

		// authed — session JWT only
		r.Group(func(r chi.Router) {
			// §23: API-key bearer resolves before session auth (immediate
			// 401 on revoked/invalid keys; falls through to JWT otherwise).
			r.Use(middleware.APIKeyAuth(intSvc))
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
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage"), middleware.RateLimit(vk, "platform", 10, time.Minute)).Get("/platform/tenants/{tenant_id}/export", platH.ExportTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage"), middleware.RateLimit(vk, "platform", 10, time.Minute)).Post("/platform/tenants/{tenant_id}/export", platH.StartTenantExport)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Get("/platform/tenants/{tenant_id}/exports/{export_id}", platH.DownloadTenantExport)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage"), middleware.RateLimit(vk, "platform", 30, time.Minute)).Post("/platform/tenants", platH.CreateTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Get("/platform/provisions/{request_id}", platH.ProvisionStatus)
			// §23 tenant-scoped api keys — control-plane issues keys for
			// the tenant itself (user_id NULL on the row).
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Get("/platform/tenants/{tenant_id}/api-keys", intH.ListTenantKeys)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Post("/platform/tenants/{tenant_id}/api-keys", intH.IssueTenantKey)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Delete("/platform/tenants/{tenant_id}/api-keys/{key_id}", intH.RevokeTenantKey)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Post("/platform/tenants/{tenant_id}/suspend", platH.SuspendTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.tenants.manage")).Post("/platform/tenants/{tenant_id}/reinstate", platH.ReinstateTenant)
			r.With(middleware.RequirePlatformNode(pool, "platform.subscriptions.manage")).Get("/platform/subscriptions", platH.Subscriptions)
			r.With(middleware.RequirePlatformNode(pool, "platform.subscriptions.manage")).Post("/platform/subscriptions/{subscription_id}/override", platH.OverrideSubscription)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.manage")).Post("/platform/plans", platH.UpsertPlan)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.manage")).Patch("/platform/plans/{slug}", platH.UpsertPlan)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.manage")).Get("/platform/addons", billH.PlatformAddons)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.manage")).Post("/platform/addons", billH.UpsertAddon)
			r.With(middleware.RequirePlatformNode(pool, "platform.plans.manage")).Patch("/platform/addons/{slug}", billH.UpsertAddon)
			r.With(middleware.RequirePlatformNode(pool, "platform.deployments.manage")).Get("/platform/deployments", platH.Deployments)
			r.With(middleware.RequirePlatformNode(pool, "platform.deployments.manage")).Post("/platform/deployments", platH.CreateDeployment)
			r.With(middleware.RequirePlatformNode(pool, "platform.support.impersonate"), middleware.RateLimit(vk, "platform", 10, time.Minute)).Post("/platform/support/impersonate", platH.Impersonate)
			r.With(middleware.RequirePlatformNode(pool, "platform.support.impersonate")).Post("/platform/support/impersonate/{session_id}/end", platH.EndImpersonation)
			r.Get("/me/support-sessions", platH.MySupportSessions) // §15 banner — tenant-visible
			r.With(middleware.RequirePlatformNode(pool, "platform.audit.view")).Get("/platform/audit", platH.Audit)
			r.With(middleware.RequirePlatformNode(pool, "platform.analytics.view")).Get("/platform/usage", platH.Usage)
			r.With(middleware.RequirePlatformNode(pool, "platform.analytics.view")).Get("/platform/metrics", handler.Metrics(cfg.ValkeyAddr, pool))
			r.With(middleware.RequirePlatformNode(pool, "platform.ai.usage.view")).Get("/platform/ai/usage", aiH.TenantUsage)
			// §28 platform blog — control-plane authored, platform.blog.manage node
			r.With(middleware.RequirePlatformNode(pool, "platform.blog.manage")).Get("/platform/blog", blogH.AdminList)
			r.With(middleware.RequirePlatformNode(pool, "platform.blog.manage")).Post("/platform/blog", blogH.AdminCreate)
			r.With(middleware.RequirePlatformNode(pool, "platform.blog.manage")).Get("/platform/blog/{post_id}", blogH.AdminGet)
			r.With(middleware.RequirePlatformNode(pool, "platform.blog.manage")).Patch("/platform/blog/{post_id}", blogH.AdminPatch)
			r.With(middleware.RequirePlatformNode(pool, "platform.blog.manage")).Post("/platform/blog/{post_id}/publish", blogH.AdminPublish)
			r.With(middleware.RequirePlatformNode(pool, "platform.blog.manage")).Post("/platform/blog/{post_id}/unpublish", blogH.AdminUnpublish)
			r.With(middleware.RequirePlatformNode(pool, "platform.blog.manage")).Delete("/platform/blog/{post_id}", blogH.AdminDelete)
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
			// §07 add-ons: tenant-side subscribe/cancel (org.admin.billing —
			// tenantFor resolves it server-side, Rule 24).
			r.Get("/billing/addons", billH.Addons)
			r.Post("/billing/addons/{slug}/subscribe", billH.SubscribeAddon)
			r.Post("/billing/addons/{slug}/cancel", billH.CancelAddon)

			// §04/§10 tenancy + organization core
			r.Get("/me/organizations", orgH.MyOrgs)
			r.Get("/me/memberships", memberH.MyMemberships)
			r.Get("/me/finance", finH.MyFinance) // cross-org member view
			r.Get("/me/claims", finH.MyClaims)
			r.Get("/me/calendar", actH.MyCalendar)
			r.Get("/me/volunteer-signups", actH.MyVolunteerSignups)
			r.Get("/me/action-items", actH.MyItems)
			r.Get("/me/notifications", commsH.MyNotifications)
			r.Get("/me/welfare", welH.MyCases)    // §17 member's own cases
			r.Get("/me/discipline", govH.MyCases) // §16 member's own discipline cases
			r.Get("/me/analytics", anaH.Me)       // §19 member analytics
			r.Get("/me/api-keys", intH.ListKeys)  // §23 api keys
			r.Post("/me/api-keys", intH.IssueKey)
			r.Delete("/me/api-keys/{key_id}", intH.RevokeKey)
			r.Get("/me/ai/usage", aiH.MyUsage) // §22 usage meter
			r.Post("/me/notifications/{notification_id}/read", commsH.ReadNotification)
			r.Get("/me/preferences", commsH.GetPrefs)
			r.With(middleware.RateLimit(vk, "ws_ticket", 10, time.Minute)).Post("/ws/ticket", wsGw.Ticket)

			r.Patch("/me/preferences", commsH.SetPrefs)
			r.Get("/org-templates", orgH.Templates)
			r.Post("/orgs", orgH.Create)
			// ADR-026 tenant export/migration — owner-org admin only.
			r.With(middleware.RateLimit(vk, "tenant_export", 5, time.Hour)).Post("/tenants/{tenant_id}/exports", texH.Create)
			r.Get("/tenants/{tenant_id}/exports/{export_id}", texH.Get)
			r.With(middleware.RateLimit(vk, "tenant_export_confirm", 10, time.Hour)).Post("/tenants/{tenant_id}/exports/{export_id}/confirm", texH.Confirm)
			r.With(middleware.RateLimit(vk, "invite_accept", 10, time.Minute)).Post("/invitations/{token}/accept", orgH.Accept)
			r.With(middleware.RateLimit(vk, "invite_otp", 5, time.Minute)).Post("/invitations/{token}/otp", orgH.RequestInviteOTP)
			// applicant self-service — no OrgCtx (a pre-decision applicant
			// has no membership); the handler verifies ownership.
			r.Post("/orgs/{org_id}/applications/{application_id}/withdraw", memberH.Withdraw)
			// member self-resign — suspended members carry no OrgCtx but may
			// still exit (§11); the service resolves their membership.
			r.Post("/orgs/{org_id}/resign", memberH.Resign)
			r.Group(func(r chi.Router) {
				r.Use(middleware.RequireOrgCtx(orgSvc))
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}", orgH.Get)
				r.Get("/orgs/{org_id}/me", orgH.Me)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Patch("/orgs/{org_id}", orgH.Patch)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/structure", orgH.Structure)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/units", orgH.Units)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/fields/groups", fieldsH.Groups)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.fields.manage")).Post("/orgs/{org_id}/fields/groups", fieldsH.CreateGroup)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.fields.manage")).Patch("/orgs/{org_id}/fields/groups/{group_id}", fieldsH.PatchGroup)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.fields.manage")).Delete("/orgs/{org_id}/fields/groups/{group_id}", fieldsH.DeleteGroup)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/fields", fieldsH.Fields)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.fields.manage")).Post("/orgs/{org_id}/fields", fieldsH.CreateField)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.fields.manage")).Patch("/orgs/{org_id}/fields/{field_id}", fieldsH.PatchField)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.fields.manage")).Delete("/orgs/{org_id}/fields/{field_id}", fieldsH.DeleteField)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.view")).Get("/orgs/{org_id}/fields/schema", fieldsH.Schema)
				r.Patch("/orgs/{org_id}/fields/values", fieldsH.SetValues) // entity-scoped — inside
				r.With(middleware.RequireOrgPermission(orgSvc, "org.structure.manage")).Post("/orgs/{org_id}/units", orgH.CreateUnit)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.structure.manage")).Patch("/orgs/{org_id}/units/{unit_id}", orgH.UpdateUnit)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.structure.manage")).Delete("/orgs/{org_id}/units/{unit_id}", orgH.ArchiveUnit)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Get("/orgs/{org_id}/settings", orgH.Settings)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Get("/orgs/{org_id}/support-sessions", platH.OrgSupportSessions)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Patch("/orgs/{org_id}/settings", orgH.SetSetting)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.invite")).Get("/orgs/{org_id}/invitations", orgH.Invitations)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.invite")).Post("/orgs/{org_id}/invitations", orgH.Invite)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.members.invite")).Delete("/orgs/{org_id}/invitations/{invitation_id}", orgH.RevokeInvitation)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.branding")).Patch("/orgs/{org_id}/branding", orgH.Branding)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Get("/orgs/{org_id}/apps", appsH.OrgApps)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Post("/orgs/{org_id}/apps/{app_id}", appsH.Activate)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.admin.settings")).Delete("/orgs/{org_id}/apps/{app_id}", appsH.Deactivate)
				// §16 API table: governance reads gate on org.governance.view.
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/positions", orgH.ListPositions)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/terms", orgH.ListTerms)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/appointments", orgH.ListAppointments)
				r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/units/{unit_id}/members", orgH.ListMembers)
				// §16: positions/terms/appointments/committee roster are
				// governance-app surfaces — gated like the rest of §16.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "governance"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.positions.manage")).Post("/orgs/{org_id}/positions", orgH.CreatePosition)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.terms.manage")).Post("/orgs/{org_id}/terms", orgH.CreateTerm)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.terms.manage")).Post("/orgs/{org_id}/terms/{term_id}/close", orgH.CloseTerm)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.appointments.manage")).Post("/orgs/{org_id}/appointments", orgH.Appoint)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.appointments.manage")).Delete("/orgs/{org_id}/appointments/{appointment_id}", orgH.EndAppointment)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.committees.manage")).Post("/orgs/{org_id}/units/{unit_id}/members", orgH.AddCommitteeMember)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.committees.manage")).Delete("/orgs/{org_id}/units/{unit_id}/members/{cm_id}", orgH.RemoveCommitteeMember)
				})

				// §16 governance — gated on the `governance` app (§06).
				// Motions → resolutions, constitution versions, committee
				// mandates, handover report.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "governance"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/motions", govH.ListMotions)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.resolutions.record")).Post("/orgs/{org_id}/motions", govH.CreateMotion)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.resolutions.record")).Post("/orgs/{org_id}/motions/{motion_id}/vote", govH.VoteMotion)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/resolutions", govH.ListResolutions)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/constitution", govH.ListConstitution)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.constitution.manage")).Post("/orgs/{org_id}/constitution", govH.AdoptConstitution)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/units/{unit_id}/mandate", govH.GetMandate)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.committees.manage")).Put("/orgs/{org_id}/units/{unit_id}/mandate", govH.UpsertMandate)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/mandates", govH.ListMandates)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.view")).Get("/orgs/{org_id}/handover", govH.Handover)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.terms.manage")).Post("/orgs/{org_id}/handover/{term_id}/complete", govH.CompleteHandover)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.resolutions.record")).Post("/orgs/{org_id}/motions/{motion_id}/debate", govH.DebateMotion)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.governance.resolutions.record")).Post("/orgs/{org_id}/motions/{motion_id}/withdraw", govH.WithdrawMotion)
					// §16 discipline casework — investigation → hearing →
					// resolution → appeal → sanction.
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.view")).Get("/orgs/{org_id}/discipline/cases", govH.ListCases)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.manage")).Post("/orgs/{org_id}/discipline/cases", govH.OpenCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.view")).Get("/orgs/{org_id}/discipline/cases/{case_id}", govH.GetCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.manage")).Patch("/orgs/{org_id}/discipline/cases/{case_id}", govH.AdvanceCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.view")).Get("/orgs/{org_id}/discipline/cases/{case_id}/hearings", govH.ListHearings)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.hear")).Post("/orgs/{org_id}/discipline/cases/{case_id}/hearings", govH.ScheduleHearing)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.hear")).Post("/orgs/{org_id}/discipline/cases/{case_id}/hearings/{hearing_id}/complete", govH.CompleteHearing)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.view")).Get("/orgs/{org_id}/discipline/cases/{case_id}/appeals", govH.ListAppeals)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.hear")).Post("/orgs/{org_id}/discipline/cases/{case_id}/resolve", govH.ResolveCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.appeal_own")).Post("/orgs/{org_id}/discipline/cases/{case_id}/appeals", govH.FileAppeal)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.discipline.hear")).Post("/orgs/{org_id}/discipline/appeals/{appeal_id}/decide", govH.DecideAppeal)
				})

				// §17 welfare — cases, approvals, disbursement, visits.
				// Gated on the `welfare` app (requires finance_lite too —
				// the app graph resolves that on activation).
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "welfare"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.view_any")).Get("/orgs/{org_id}/welfare/cases", welH.ListCases)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.request_own")).Post("/orgs/{org_id}/welfare/cases", welH.CreateCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.view_any")).Get("/orgs/{org_id}/welfare/cases/{case_id}", welH.GetCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.review")).Patch("/orgs/{org_id}/welfare/cases/{case_id}", welH.ReviewCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.approve")).Post("/orgs/{org_id}/welfare/cases/{case_id}/decision", welH.DecideCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.disburse")).Post("/orgs/{org_id}/welfare/cases/{case_id}/disburse", welH.DisburseCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.visit")).Post("/orgs/{org_id}/welfare/cases/{case_id}/visits", welH.ScheduleVisit)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.visit")).Post("/orgs/{org_id}/welfare/cases/{case_id}/visits/{visit_id}/complete", welH.CompleteVisit)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.review")).Post("/orgs/{org_id}/welfare/cases/{case_id}/close", welH.CloseCase)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.welfare.view_any")).Get("/orgs/{org_id}/welfare/policies", welH.Policies)
				})

				// §19 analytics — role dashboards, health score, reports,
				// consolidation (business tier, gated inside the service).
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "analytics"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.analytics.view")).Get("/orgs/{org_id}/analytics/dashboard", anaH.Dashboard)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.analytics.view")).Get("/orgs/{org_id}/analytics/health", anaH.Health)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.analytics.view")).Get("/orgs/{org_id}/reports", anaH.ListReports)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.analytics.reports.generate")).Post("/orgs/{org_id}/reports", anaH.RunReport)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.analytics.export")).Get("/orgs/{org_id}/reports/{report_id}/download", anaH.DownloadReport)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.analytics.consolidated")).Get("/orgs/{org_id}/analytics/consolidated", anaH.Consolidated)
				})

				// §18 assets — registry, custody, maintenance, audits.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "assets"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.view")).Get("/orgs/{org_id}/assets", astH.List)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.register")).Post("/orgs/{org_id}/assets", astH.Register)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.view")).Get("/orgs/{org_id}/assets/{asset_id}", astH.Get)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.register")).Patch("/orgs/{org_id}/assets/{asset_id}", astH.Patch)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.assign")).Post("/orgs/{org_id}/assets/{asset_id}/assign", astH.Assign)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.assign")).Post("/orgs/{org_id}/assets/{asset_id}/return", astH.Return)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.maintain")).Post("/orgs/{org_id}/assets/{asset_id}/maintenance", astH.Maintenance)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.dispose")).Post("/orgs/{org_id}/assets/{asset_id}/dispose", astH.Dispose)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.audit")).Get("/orgs/{org_id}/assets/audits", astH.ListAudits)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.audit")).Post("/orgs/{org_id}/assets/audits", astH.StartAudit)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.audit")).Post("/orgs/{org_id}/assets/audits/{audit_id}/complete", astH.CompleteAudit)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.view")).Get("/orgs/{org_id}/assets/{asset_id}/credentials", astH.ListCredentials)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.assets.credentials.manage")).Post("/orgs/{org_id}/assets/{asset_id}/credentials", astH.SaveCredential)
				})

				// §20 projects — resolution-gated delivery.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "projects"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.view")).Get("/orgs/{org_id}/projects", prjH.List)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.propose")).Post("/orgs/{org_id}/projects", prjH.Propose)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.view")).Get("/orgs/{org_id}/projects/{project_id}", prjH.Get)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.manage")).Post("/orgs/{org_id}/projects/{project_id}/transition", prjH.Transition)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.approve")).Post("/orgs/{org_id}/projects/{project_id}/approve", prjH.Approve)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.view")).Get("/orgs/{org_id}/projects/{project_id}/milestones", prjH.ListMilestones)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.manage")).Post("/orgs/{org_id}/projects/{project_id}/milestones", prjH.AddMilestone)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.manage")).Post("/orgs/{org_id}/projects/{project_id}/milestones/{milestone_id}/complete", prjH.CompleteMilestone)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.update")).Post("/orgs/{org_id}/projects/{project_id}/updates", prjH.PostUpdate)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.view")).Get("/orgs/{org_id}/projects/{project_id}/updates", prjH.ListUpdates)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.vendors.manage")).Get("/orgs/{org_id}/vendors", prjH.ListVendors)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.vendors.manage")).Post("/orgs/{org_id}/vendors", prjH.AddVendor)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.manage")).Post("/orgs/{org_id}/projects/{project_id}/donors", prjH.RecordDonor)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.manage"), middleware.RequireOrgPermission(orgSvc, "org.finance.expenses.disburse")).Post("/orgs/{org_id}/projects/{project_id}/spend", prjH.RecordSpend)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.manage")).Post("/orgs/{org_id}/projects/{project_id}/budget", prjH.SetBudget)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.view")).Get("/orgs/{org_id}/projects/{project_id}/budget", prjH.GetBudget)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.view")).Get("/orgs/{org_id}/projects/{project_id}/graph", prjH.Graph)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.projects.manage")).Post("/orgs/{org_id}/projects/{project_id}/close", prjH.Close)
				})

				// §21 elections — nominations → secret ballot → ratification.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "elections"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.results.view")).Get("/orgs/{org_id}/elections", elcH.List)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.manage")).Post("/orgs/{org_id}/elections", elcH.Create)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.results.view")).Get("/orgs/{org_id}/elections/{election_id}", elcH.Get)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.manage")).Patch("/orgs/{org_id}/elections/{election_id}", elcH.Patch)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.nominate")).Post("/orgs/{org_id}/elections/{election_id}/nominate", elcH.Nominate)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.vet")).Post("/orgs/{org_id}/elections/{election_id}/candidates/{candidate_id}/vet", elcH.Vet)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.vote_own")).Get("/orgs/{org_id}/elections/{election_id}/ballot", elcH.Ballot)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.vote_own")).Post("/orgs/{org_id}/elections/{election_id}/vote", elcH.Vote)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.results.view")).Get("/orgs/{org_id}/elections/{election_id}/results", elcH.Results)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.ratify")).Post("/orgs/{org_id}/elections/{election_id}/ratify", elcH.Ratify)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.manage")).Post("/orgs/{org_id}/elections/{election_id}/annul", elcH.Annul)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.elections.audit.view")).Get("/orgs/{org_id}/elections/{election_id}/audit", elcH.Audit)
				})

				// §23 integrations — connectors + ICS feed (org-scoped).
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "integrations"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.integrations.view")).Get("/orgs/{org_id}/integrations/connectors", intH.ListConnectors)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.integrations.configure")).Post("/orgs/{org_id}/integrations/connectors", intH.Configure)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.integrations.configure")).Patch("/orgs/{org_id}/integrations/connectors/{connector_id}", intH.SetEnabled)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.integrations.view")).Get("/orgs/{org_id}/calendar.ics", intH.Calendar)
				})

				// §22 ai — permission-scoped chat + HITL generations.
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "ai-assistant"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.ai.ask")).Post("/orgs/{org_id}/ai/chat", aiH.Chat)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.ai.ask")).Get("/orgs/{org_id}/ai/conversations", aiH.Conversations)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.ai.ask")).Get("/orgs/{org_id}/ai/conversations/{conversation_id}", aiH.Messages)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.ai.drafts.manage")).Post("/orgs/{org_id}/ai/drafts", aiH.Draft)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.ai.drafts.manage")).Get("/orgs/{org_id}/ai/drafts", aiH.ListDrafts)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.ai.drafts.manage")).Post("/orgs/{org_id}/ai/drafts/{generation_id}/review", aiH.Review)
				})

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
					// §25 finance full — budgets, expenses, invoices.
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/budgets", finH.Budgets)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.budgets.manage")).Post("/orgs/{org_id}/finance/budgets", finH.CreateBudget)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/budgets/{budget_id}", finH.GetBudget)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.budgets.manage")).Patch("/orgs/{org_id}/finance/budgets/{budget_id}", finH.PatchBudget)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.budgets.manage")).Post("/orgs/{org_id}/finance/budgets/{budget_id}/adopt", finH.AdoptBudget)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.budgets.manage")).Post("/orgs/{org_id}/finance/budgets/{budget_id}/close", finH.CloseBudget)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/budgets/{budget_id}/vs-actual", finH.VsActual)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/expenses", finH.Expenses)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.expenses.request")).Post("/orgs/{org_id}/finance/expenses", finH.RequestExpense)
					r.Get("/orgs/{org_id}/finance/expenses/{expense_id}", finH.GetExpense)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.expenses.approve")).Post("/orgs/{org_id}/finance/expenses/{expense_id}/approve", finH.ApproveExpense)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.expenses.approve")).Post("/orgs/{org_id}/finance/expenses/{expense_id}/reject", finH.RejectExpense)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.expenses.disburse")).Post("/orgs/{org_id}/finance/expenses/{expense_id}/disburse", finH.DisburseExpense)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.reports.read")).Get("/orgs/{org_id}/finance/invoices", finH.Invoices)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.invoices.manage")).Post("/orgs/{org_id}/finance/invoices", finH.CreateInvoice)
					r.Get("/orgs/{org_id}/finance/invoices/{invoice_id}", finH.GetInvoice)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.finance.invoices.manage")).Post("/orgs/{org_id}/finance/invoices/{invoice_id}/void", finH.VoidInvoice)
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
					// §27 volunteers — roles on activities + signup lifecycle.
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.view")).Get("/orgs/{org_id}/activities/{activity_id}/volunteer-roles", actH.VolunteerRoles)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.manage")).Post("/orgs/{org_id}/activities/{activity_id}/volunteer-roles", actH.CreateVolunteerRole)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.manage")).Patch("/orgs/{org_id}/activities/{activity_id}/volunteer-roles/{role_id}", actH.PatchVolunteerRole)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.manage")).Delete("/orgs/{org_id}/activities/{activity_id}/volunteer-roles/{role_id}", actH.DeleteVolunteerRole)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.signup_own")).Post("/orgs/{org_id}/activities/{activity_id}/volunteer-roles/{role_id}/signup", actH.VolunteerSignup)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.signup_own")).Delete("/orgs/{org_id}/activities/{activity_id}/volunteer-signups/{signup_id}", actH.WithdrawSignup)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.manage")).Post("/orgs/{org_id}/activities/{activity_id}/volunteer-signups/{signup_id}/confirm", actH.ConfirmSignup)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.manage")).Post("/orgs/{org_id}/activities/{activity_id}/volunteer-signups/{signup_id}/mark", actH.MarkSignup)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.volunteers.view")).Get("/orgs/{org_id}/activities/{activity_id}/volunteer-roster", actH.VolunteerRoster)
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
					// §26 polls & surveys — consultative member voice.
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.view")).Get("/orgs/{org_id}/polls", commsH.Polls)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.polls.manage")).Post("/orgs/{org_id}/polls", commsH.CreatePoll)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.view")).Get("/orgs/{org_id}/polls/{poll_id}", commsH.GetPoll)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.polls.manage")).Post("/orgs/{org_id}/polls/{poll_id}/open", commsH.OpenPoll)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.polls.vote_own")).Post("/orgs/{org_id}/polls/{poll_id}/vote", commsH.Vote)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.polls.manage")).Post("/orgs/{org_id}/polls/{poll_id}/close", commsH.ClosePoll)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.polls.manage")).Get("/orgs/{org_id}/polls/{poll_id}/results", commsH.PollResults)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.view")).Get("/orgs/{org_id}/surveys", commsH.Surveys)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.surveys.manage")).Post("/orgs/{org_id}/surveys", commsH.CreateSurvey)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.view")).Get("/orgs/{org_id}/surveys/{survey_id}", commsH.GetSurvey)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.surveys.manage")).Post("/orgs/{org_id}/surveys/{survey_id}/open", commsH.OpenSurvey)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.surveys.respond_own")).Post("/orgs/{org_id}/surveys/{survey_id}/respond", commsH.Respond)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.surveys.manage")).Post("/orgs/{org_id}/surveys/{survey_id}/close", commsH.CloseSurvey)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.comms.surveys.manage")).Get("/orgs/{org_id}/surveys/{survey_id}/results", commsH.SurveyResults)
				})

				// §24 voice rooms — staged app: gated on `voice-rooms`
				// (its requires:[comms_documents] resolves via the §06 app
				// dependency graph on activation).
				r.Group(func(r chi.Router) {
					r.Use(middleware.RequireAppActive(orgSvc, "voice-rooms"))
					r.With(middleware.RequireOrgPermission(orgSvc, "org.chat.use")).Get("/orgs/{org_id}/voice-rooms", chatH.LiveRooms)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.voice.start")).Post("/orgs/{org_id}/voice-rooms", chatH.StartRoom)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.voice.join")).Post("/orgs/{org_id}/voice-rooms/{room_id}/join", chatH.JoinRoom)
					r.With(middleware.RequireOrgPermission(orgSvc, "org.voice.join")).Post("/orgs/{org_id}/voice-rooms/{room_id}/leave", chatH.LeaveRoom)
					// end = room starter or moderator — the starter check
					// is inside EndRoom; the route admits plausible actors.
					r.With(middleware.RequireAnyOrgPermission(orgSvc, "org.voice.start", "org.chat.moderate")).Post("/orgs/{org_id}/voice-rooms/{room_id}/end", chatH.EndRoom)
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
