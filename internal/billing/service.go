// Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
// Subscriptions + lifecycle machine, self-hosted licenses (Rule 29), and
// Rule 9 billing webhooks (durable record → async processing). Every
// transition emits SubscriptionChanged in the same transaction.
package billing

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
)

var (
	ErrNoSubscription      = errors.New("subscription_not_found")
	ErrSubscriptionExists  = errors.New("subscription_exists")
	ErrPlanNotFound        = errors.New("plan_not_found")
	ErrBadBillingTerm      = errors.New("invalid_billing_term")
	ErrBadTransition       = errors.New("invalid_state_transition")
	ErrDuplicateEvent      = errors.New("duplicate_webhook_event")
	ErrBadSignature        = errors.New("invalid_signature")
	ErrLicenseNotFound     = errors.New("license_not_found")
	ErrLicenseExpired      = errors.New("license_expired")
	ErrBadExpiry           = errors.New("invalid_expiry")
	ErrTenantNotFound      = errors.New("tenant_not_found")
	ErrWebhookUnconfigured = errors.New("webhook_provider_not_configured")
	ErrBadPayload          = errors.New("invalid_webhook_payload")
	ErrBadValue            = errors.New("bad_value")
	ErrNotFound            = errors.New("not_found")
	ErrAddonNotEligible    = errors.New("addon_not_eligible")
	// ErrPaymentRequired: self-serve billing covers the free plan and
	// downgrades only — upgrades flow through the provider webhook or a
	// platform override (§15), never a bare plan_slug write.
	ErrPaymentRequired = errors.New("payment_required")
)

var billingTerms = map[string]bool{"monthly": true, "annual": true, "enterprise_contract": true}

type Service struct {
	pool          *pgxpool.Pool
	q             *db.Queries
	signKey       ed25519.PrivateKey // from OGA9_LICENSE_SIGNING_KEY seed
	webhookSecret []byte             // OGA9_BILLING_WEBHOOK_SECRET
}

// NewService: signSeed is a 32-byte ed25519 seed (config.LicenseSignKey).
// Self-hosted deployments hold only the public half — signed files verify
// offline without embedding private material (Rule 29).
func NewService(pool *pgxpool.Pool, signSeed, webhookSecret []byte) *Service {
	var priv ed25519.PrivateKey
	if len(signSeed) == ed25519.SeedSize {
		priv = ed25519.NewKeyFromSeed(signSeed)
	}
	return &Service{pool: pool, q: db.New(pool), signKey: priv, webhookSecret: webhookSecret}
}

func toPg(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func pgTime(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) settingInt(ctx context.Context, key string, fallback int64) int64 {
	v, err := s.q.GetSystemSetting(ctx, key)
	if err != nil {
		return fallback
	}
	var n json.Number
	if json.Unmarshal(v, &n) != nil {
		return fallback
	}
	i, err := n.Int64()
	if err != nil {
		return fallback
	}
	return i
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// ---------- subscriptions ----------

// ListPlans returns public plans for the plan-picker / platform views.
func (s *Service) ListPlans(ctx context.Context) ([]db.ListPlansRow, error) {
	return s.q.ListPlans(ctx)
}

func (s *Service) GetSubscription(ctx context.Context, tenantID uuid.UUID) (*db.Subscriptions, error) {
	sub, err := s.q.GetSubscription(ctx, toPg(tenantID))
	if err == pgx.ErrNoRows {
		return nil, ErrNoSubscription
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// selfServePrice reports the plan's price for the billing term. ok=false
// means the plan/term isn't self-serve priced (unlisted plan, NULL price,
// or enterprise_contract) — those need a provider checkout or a platform
// override, never a bare slug write.
func selfServePrice(p db.GetPlanRow, term string) (int64, bool) {
	if !p.IsPublic || term == "enterprise_contract" {
		return 0, false
	}
	v := p.PriceMonthlyMinor
	if term == "annual" {
		v = p.PriceAnnualMinor
	}
	if !v.Valid {
		return 0, false
	}
	return v.Int64, true
}

// CreateSubscription starts a plan on trial (billing.trial_days, Rule 3) —
// used both at tenant provision and for POST /billing/subscription on a
// tenant with no current subscription. Self-serve is the free plan only;
// paid plans need a provider checkout (payment_required).
func (s *Service) CreateSubscription(ctx context.Context, tenantID uuid.UUID, plan, term, actorID string) (uuid.UUID, error) {
	p, err := s.q.GetPlan(ctx, plan)
	if err == pgx.ErrNoRows {
		return uuid.Nil, ErrPlanNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	if !billingTerms[term] {
		return uuid.Nil, ErrBadBillingTerm
	}
	if price, ok := selfServePrice(p, term); !ok || price > 0 {
		return uuid.Nil, ErrPaymentRequired
	}
	trialDays := s.settingInt(ctx, "billing.trial_days", 14)
	trialEnd := time.Now().Add(time.Duration(trialDays) * 24 * time.Hour)
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		sid, err := q.CreateSubscription(ctx, db.CreateSubscriptionParams{
			TenantID: toPg(tenantID), PlanSlug: plan, State: "trial",
			BillingTerm: term, TrialEndsAt: pgTime(trialEnd),
		})
		if err != nil {
			if isUniqueViolation(err) {
				return ErrSubscriptionExists // subscriptions_one_open (000059)
			}
			return err
		}
		id = uuid.UUID(sid.Bytes)
		uid := uuid.Nil
		if u, err := uuid.Parse(actorID); err == nil {
			uid = u
		}
		price, _ := selfServePrice(p, term) // gated above: always 0 on this path
		if err := s.issueInvoice(ctx, q, tenantID, id, "", price, p.Currency); err != nil {
			return err
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "SubscriptionChanged", "subscription", id, uid, true,
			map[string]any{"tenant_id": tenantID, "from": "", "to": "trial", "plan_slug": plan})
	})
	return id, err
}

// ProvisionTrial starts the starter-plan trial for a brand-new tenant — runs
// inside the provisioning tx (called from CreateOrg).
func ProvisionTrial(ctx context.Context, q *db.Queries, tenantID, actorID uuid.UUID, trialDays int64) error {
	trialEnd := time.Now().Add(time.Duration(trialDays) * 24 * time.Hour)
	sid, err := q.CreateSubscription(ctx, db.CreateSubscriptionParams{
		TenantID: toPg(tenantID), PlanSlug: "starter", State: "trial",
		BillingTerm: "monthly", TrialEndsAt: pgTime(trialEnd),
	})
	if err != nil {
		return err
	}
	return events.Emit(ctx, q, tenantID, uuid.Nil, "SubscriptionChanged", "subscription",
		uuid.UUID(sid.Bytes), actorID, true,
		map[string]any{"tenant_id": tenantID, "from": "", "to": "trial", "plan_slug": "starter"})
}

// transition moves the subscription one step and emits SubscriptionChanged
// in the same tx. graceEnd/cancelledAt zero-value → column untouched.
// The write is compare-and-set on the read state — a concurrent transition
// that already moved the row makes this a no-op (no regressed state, no
// phantom event).
func (s *Service) transition(ctx context.Context, q *db.Queries, sub *db.Subscriptions, to string, graceEnd, cancelledAt time.Time, actor uuid.UUID) error {
	from := sub.State
	if from == to {
		return nil
	}
	n, err := q.TransitionSubscriptionState(ctx, db.TransitionSubscriptionStateParams{
		ID: sub.ID, ToState: to, FromState: from,
		GraceEndsAt: pgTime(graceEnd), CancelledAt: pgTime(cancelledAt),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return nil // another writer moved it past `from` — nothing to emit
	}
	return events.Emit(ctx, q, uuid.UUID(sub.TenantID.Bytes), uuid.Nil, "SubscriptionChanged",
		"subscription", uuid.UUID(sub.ID.Bytes), actor, true,
		map[string]any{"tenant_id": uuid.UUID(sub.TenantID.Bytes), "from": from, "to": to, "plan_slug": sub.PlanSlug})
}

// ChangePlan swaps the plan slug. Self-serve is restricted to free or
// strictly-cheaper plans (downgrade/sidegrade) — upgrades return
// ErrPaymentRequired and land via provider webhook or §15 platform
// override. Proration is a §15 concern.
func (s *Service) ChangePlan(ctx context.Context, tenantID uuid.UUID, plan string, actor uuid.UUID) error {
	sub, err := s.GetSubscription(ctx, tenantID)
	if err != nil {
		return err
	}
	target, err := s.q.GetPlan(ctx, plan)
	if err == pgx.ErrNoRows {
		return ErrPlanNotFound
	}
	if err != nil {
		return err
	}
	if sub.PlanSlug == plan {
		// Re-selecting the current plan cancels a pending scheduled
		// downgrade — without this the stale schedule still fires at
		// period end (a "stay" that downgrades anyway).
		if sub.ScheduledPlanSlug.Valid && sub.ScheduledPlanSlug.String != "" {
			return s.withTx(ctx, func(q *db.Queries) error {
				if err := q.ClearScheduledPlan(ctx, sub.ID); err != nil {
					return err
				}
				return events.Emit(ctx, q, uuid.UUID(sub.TenantID.Bytes), uuid.Nil, "SubscriptionChanged",
					"subscription", uuid.UUID(sub.ID.Bytes), actor, true,
					map[string]any{"tenant_id": uuid.UUID(sub.TenantID.Bytes),
						"from": sub.State, "to": sub.State, "plan_slug": sub.PlanSlug,
						"cleared_scheduled": sub.ScheduledPlanSlug.String})
			})
		}
		return nil
	}
	if sub.State == "cancelled" {
		return ErrBadTransition // cancelled subs can't change plan — resubscribe
	}
	cur, err := s.q.GetPlan(ctx, sub.PlanSlug)
	if err != nil {
		return err
	}
	targetPrice, targetOK := selfServePrice(target, sub.BillingTerm)
	curPrice, curOK := selfServePrice(cur, sub.BillingTerm)
	// Free/sidegrade/downgrade only: a paid target needs payment whenever the
	// current plan can't prove the move is strictly cheaper — an unpriced or
	// unlisted current plan offers no comparison (never a bare slug write).
	if !targetOK || (targetPrice > 0 && (!curOK || targetPrice > curPrice)) {
		return ErrPaymentRequired
	}
	// §15: a downgrade on an ACTIVE subscription schedules at period end —
	// the tenant keeps the paid tier until current_period_end, then the
	// sweep applies it. Trials/sidegrades (and past-due rows already off
	// the paid period) apply immediately.
	if curOK && targetPrice < curPrice && sub.State == "active" {
		return s.withTx(ctx, func(q *db.Queries) error {
			n, err := q.ScheduleSubscriptionPlan(ctx, db.ScheduleSubscriptionPlanParams{
				ID: sub.ID, PlanSlug: pgtype.Text{String: plan, Valid: true}, ExpectedSlug: sub.PlanSlug,
			})
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrBadTransition // raced or no longer active
			}
			return events.Emit(ctx, q, uuid.UUID(sub.TenantID.Bytes), uuid.Nil, "PlanChangeScheduled",
				"subscription", uuid.UUID(sub.ID.Bytes), actor, true,
				map[string]any{"tenant_id": uuid.UUID(sub.TenantID.Bytes),
					"from_plan": sub.PlanSlug, "plan_slug": plan,
					"effective_at": sub.CurrentPeriodEnd.Time})
		})
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetSubscriptionPlan(ctx, db.SetSubscriptionPlanParams{
			ID: sub.ID, PlanSlug: plan, ExpectedSlug: sub.PlanSlug,
		})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadTransition // raced with another plan change or a cancel
		}
		if err := s.issueInvoice(ctx, q, tenantID, uuid.UUID(sub.ID.Bytes), "", targetPrice, target.Currency); err != nil {
			return err
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "SubscriptionChanged", "subscription",
			uuid.UUID(sub.ID.Bytes), actor, true,
			map[string]any{"tenant_id": tenantID, "from": sub.State, "to": sub.State,
				"plan_slug": plan, "from_plan": sub.PlanSlug})
	})
}

// Cancel ends the subscription — records stay (Rule 28).
func (s *Service) Cancel(ctx context.Context, tenantID uuid.UUID, actor uuid.UUID) error {
	sub, err := s.GetSubscription(ctx, tenantID)
	if err != nil {
		return err
	}
	if sub.State == "cancelled" {
		return nil // idempotent
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.ClearScheduledPlan(ctx, sub.ID); err != nil {
			return err
		}
		return s.transition(ctx, q, sub, "cancelled", time.Time{}, time.Now(), actor)
	})
}

// activate marks the subscription active (webhook: payment succeeded) and
// extends the period end. Cancelled stays cancelled — a late invoice.paid
// can't resurrect a terminal subscription (ActivateSubscription guards).
func (s *Service) activate(ctx context.Context, q *db.Queries, sub *db.Subscriptions) error {
	period := 30 * 24 * time.Hour
	if sub.BillingTerm == "annual" || sub.BillingTerm == "enterprise_contract" {
		period = 365 * 24 * time.Hour
	}
	n, err := q.ActivateSubscription(ctx, db.ActivateSubscriptionParams{
		ID: sub.ID, CurrentPeriodEnd: pgTime(time.Now().Add(period)),
	})
	if err != nil {
		return err
	}
	if n == 0 {
		return nil // cancelled — terminal; no state change, no event
	}
	return events.Emit(ctx, q, uuid.UUID(sub.TenantID.Bytes), uuid.Nil, "SubscriptionChanged",
		"subscription", uuid.UUID(sub.ID.Bytes), uuid.Nil, true,
		map[string]any{"tenant_id": uuid.UUID(sub.TenantID.Bytes), "from": sub.State, "to": "active", "plan_slug": sub.PlanSlug})
}

// SweepStates drives the time-based lifecycle transitions (§07):
//
//	trial expired      → grace (fresh grace window from billing.grace_period_days)
//	active period end  → past_due (silent provider must lapse, not free-ride)
//	grace/past_due exp → restricted
//
// cancelled is manual-only (webhook or route). Emits SubscriptionChanged per
// transition inside its own tx — one bad row can't block the sweep.
func (s *Service) SweepStates(ctx context.Context) error {
	rows, err := s.q.ListDueSubscriptions(ctx)
	if err != nil {
		return err
	}
	graceDays := s.settingInt(ctx, "billing.grace_period_days", 14)
	for _, r := range rows {
		sub := db.Subscriptions{
			ID: r.ID, TenantID: r.TenantID, PlanSlug: r.PlanSlug, State: r.State,
		}
		var to string
		var graceEnd time.Time
		switch r.State {
		case "trial":
			to, graceEnd = "grace", time.Now().Add(time.Duration(graceDays)*24*time.Hour)
		case "active":
			to, graceEnd = "past_due", time.Now().Add(time.Duration(graceDays)*24*time.Hour)
		case "past_due", "grace":
			to = "restricted"
		default:
			continue
		}
		err := s.withTx(ctx, func(q *db.Queries) error {
			// §15: a scheduled downgrade applies at period end — emit the
			// plan change before the state move so listeners see both.
			if r.ScheduledPlanSlug.Valid && r.ScheduledPlanSlug.String != "" {
				n, err := q.ApplyScheduledPlan(ctx, r.ID)
				if err != nil {
					return err
				}
				if n > 0 {
					if err := events.Emit(ctx, q, uuid.UUID(r.TenantID.Bytes), uuid.Nil, "SubscriptionChanged",
						"subscription", uuid.UUID(r.ID.Bytes), uuid.Nil, true,
						map[string]any{"tenant_id": uuid.UUID(r.TenantID.Bytes),
							"from_plan": r.PlanSlug, "to": r.State,
							"plan_slug": r.ScheduledPlanSlug.String}); err != nil {
						return err
					}
				}
			}
			return s.transition(ctx, q, &sub, to, graceEnd, time.Time{}, uuid.Nil)
		})
		if err != nil {
			log.Printf("subscription sweep: %s %s→%s failed: %v", r.ID.Bytes, r.State, to, err)
		}
	}
	return nil
}

// ---------- licenses (self-hosted, Rule 29) ----------

// canonicalJSON normalizes a JSON blob for signing — jsonb storage reorders
// keys and rewrites whitespace, so signatures must cover the semantic form
// (decode → re-encode with sorted keys), not the wire bytes. UseNumber
// keeps integers exact — float64 decode would corrupt limit values >2^53.
func canonicalJSON(b []byte) []byte {
	var v any
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&v); err != nil {
		return b
	}
	out, err := json.Marshal(v)
	if err != nil {
		return b
	}
	return out
}

// licensePayload encodes the signed tuple as JSON — a plain delimiter format
// would let a deployment_id containing the separator shift bytes between
// fields and forge cross-field signatures.
func (s *Service) licensePayload(deploymentID string, expiresAt time.Time, entitlements []byte) []byte {
	b, _ := json.Marshal(map[string]any{
		"deployment_id": deploymentID,
		"entitlements":  json.RawMessage(canonicalJSON(entitlements)),
		"expires_at":    expiresAt.UTC().Format(time.RFC3339),
	})
	return b
}

// sign produces an ed25519 signature — asymmetric so deployments verify
// offline against the public key without holding private material.
func (s *Service) sign(payload []byte) []byte {
	if s.signKey == nil {
		return nil
	}
	return ed25519.Sign(s.signKey, payload)
}

// IssueLicense signs + stores a self-hosted license (control-plane action,
// platform.licenses.issue). Re-issuing a deployment_id renews in place.
// Offline operation: deployments validate the signed file locally (Rule 29).
func (s *Service) IssueLicense(ctx context.Context, tenantID uuid.UUID, deploymentID, edition string, entitlements json.RawMessage, expiresAt time.Time, actor uuid.UUID) (uuid.UUID, error) {
	if !json.Valid(entitlements) {
		return uuid.Nil, errors.New("invalid_entitlements")
	}
	if !expiresAt.After(time.Now()) {
		return uuid.Nil, ErrBadExpiry
	}
	if s.signKey == nil {
		return uuid.Nil, errors.New("license_signing_unconfigured")
	}
	if _, err := s.q.GetTenant(ctx, toPg(tenantID)); err == pgx.ErrNoRows {
		return uuid.Nil, ErrTenantNotFound
	} else if err != nil {
		return uuid.Nil, err
	}
	sig := s.sign(s.licensePayload(deploymentID, expiresAt, entitlements))
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		// §07: re-issue on the same deployment_id renews in place — the
		// event distinguishes first-issue from renewal (both sourced).
		ev := "LicenseIssued"
		if _, err := q.GetLicenseByDeployment(ctx, deploymentID); err == nil {
			ev = "LicenseRenewed"
		}
		lid, err := q.InsertLicense(ctx, db.InsertLicenseParams{
			TenantID: toPg(tenantID), DeploymentID: deploymentID, Edition: edition,
			Entitlements: entitlements, ExpiresAt: pgTime(expiresAt), Signature: sig,
		})
		if err != nil {
			return err
		}
		id = uuid.UUID(lid.Bytes)
		return events.Emit(ctx, q, tenantID, uuid.Nil, ev, "license", id, actor, true,
			map[string]any{"license_id": id, "deployment_id": deploymentID})
	})
	return id, err
}

// LicenseExpirySweep — daily (license:renewal_reminder cadence): each
// license past expires_at gets LicenseExpired emitted ONCE — the
// expired_notified flag marks in the same tx (Rule 12); a renewal resets
// it so the next expiry window re-emits (§09 degrade signal).
func (s *Service) LicenseExpirySweep(ctx context.Context) (int, error) {
	rows, err := s.q.ExpiredLicenses(ctx)
	if err != nil {
		return 0, err
	}
	emitted := 0
	for _, l := range rows {
		err := s.withTx(ctx, func(q *db.Queries) error {
			if err := q.MarkLicenseExpired(ctx, l.ID); err != nil {
				return err
			}
			return events.Emit(ctx, q, uuid.UUID(l.TenantID.Bytes), uuid.Nil, "LicenseExpired",
				"license", uuid.UUID(l.ID.Bytes), uuid.Nil, true,
				map[string]any{"license_id": uuid.UUID(l.ID.Bytes), "deployment_id": l.DeploymentID})
		})
		if err != nil {
			return emitted, err
		}
		emitted++
	}
	return emitted, nil
}

// LicenseFile renders the signed license document a self-hosted deployment
// mounts at OGA9_LICENSE_FILE — the deliverable of IssueLicense; operators
// download it via GET /platform/licenses/{deployment_id}/file and ship it
// with the install. The doc verifies offline (license.Validator, Rule 29).
func (s *Service) LicenseFile(ctx context.Context, deploymentID string) ([]byte, error) {
	row, err := s.q.GetLicenseByDeployment(ctx, deploymentID)
	if err == pgx.ErrNoRows {
		return nil, ErrLicenseNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"deployment_id": row.DeploymentID,
		"edition":       row.Edition,
		"entitlements":  json.RawMessage(canonicalJSON(row.Entitlements)),
		"expires_at":    row.ExpiresAt.Time.UTC().Format(time.RFC3339),
		"signature":     hex.EncodeToString(row.Signature),
	})
}

// ValidateLicense is the self-hosted callback: the deployment presents its
// deployment_id + signature; we recompute and compare. Expiry →
// license_expired (offline grace is enforced deployment-side via
// license.offline_grace_days).
func (s *Service) ValidateLicense(ctx context.Context, deploymentID string, signatureHex string) (json.RawMessage, error) {
	row, err := s.q.GetLicenseByDeployment(ctx, deploymentID)
	if err == pgx.ErrNoRows {
		return nil, ErrLicenseNotFound
	}
	if err != nil {
		return nil, err
	}
	if time.Now().After(row.ExpiresAt.Time) {
		return nil, ErrLicenseExpired
	}
	got, err := hex.DecodeString(signatureHex)
	if err != nil || s.signKey == nil ||
		!ed25519.Verify(s.signKey.Public().(ed25519.PublicKey), s.licensePayload(deploymentID, row.ExpiresAt.Time, row.Entitlements), got) {
		return nil, ErrBadSignature
	}
	return row.Entitlements, nil
}

// ---------- webhooks (Rule 9) ----------

type webhookEnvelope struct {
	ID   string `json:"id"`
	Type string `json:"type"`
}

// RecordWebhook verifies the provider signature (HMAC-SHA256 hex over the
// raw body in X-Webhook-Signature), durably stores the event (UNIQUE
// provider,event_id = replay guard), and returns the row id for enqueue.
// A replayed event returns ErrDuplicateEvent — the endpoint still acks.
func (s *Service) RecordWebhook(ctx context.Context, provider string, body, signature []byte) (uuid.UUID, error) {
	if len(s.webhookSecret) == 0 {
		return uuid.Nil, ErrWebhookUnconfigured
	}
	m := hmac.New(sha256.New, s.webhookSecret)
	m.Write(body)
	if got := m.Sum(nil); !hmac.Equal(got, signature) {
		return uuid.Nil, ErrBadSignature
	}
	var env webhookEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.ID == "" || env.Type == "" {
		return uuid.Nil, ErrBadPayload
	}
	id, err := s.q.InsertWebhookEvent(ctx, db.InsertWebhookEventParams{
		Provider: provider, EventID: env.ID, EventType: env.Type, Payload: body,
	})
	if err == pgx.ErrNoRows {
		return uuid.Nil, ErrDuplicateEvent // already recorded — ack, don't re-drive
	}
	if err != nil {
		return uuid.Nil, err
	}
	return uuid.UUID(id.Bytes), nil
}

// ProcessWebhook is the billing:process_webhook worker — idempotent:
// already-processed events skip; unknown types are acknowledged. The event
// row is taken FOR UPDATE so concurrent processors serialize; the
// transition and processed_at mark commit atomically so a crash can't
// re-apply a transition (e.g. double-extending current_period_end).
func (s *Service) ProcessWebhook(ctx context.Context, rawEventID uuid.UUID) error {
	ev, err := s.q.GetWebhookEvent(ctx, toPg(rawEventID))
	if err != nil {
		return err
	}
	if ev.ProcessedAt.Valid {
		return nil // fast path — rechecked under the row lock
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		ev, err := q.GetWebhookEventForUpdate(ctx, toPg(rawEventID))
		if err == pgx.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if ev.ProcessedAt.Valid {
			return nil
		}
		mark := func() error {
			_, err := q.MarkWebhookProcessed(ctx, toPg(rawEventID))
			return err
		}
		var p struct {
			TenantID  string `json:"tenant_id"`
			PlanSlug  string `json:"plan_slug"`
			AddonSlug string `json:"addon_slug"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		tenantID, err := uuid.Parse(p.TenantID)
		if err != nil {
			return mark() // unrouteable — ack, don't retry forever
		}
		sub, err := q.GetSubscription(ctx, toPg(tenantID))
		if err == pgx.ErrNoRows && p.AddonSlug == "" {
			return mark() // tenant-less event — ack, don't retry forever
		}
		if err != nil {
			return err // transient lookup failure → retry, don't drop
		}
		settle := func() error {
			// Settlement lands the durable invoice as paid — addon-scoped,
			// subscription-scoped, or both as the payload carries them.
			if p.AddonSlug != "" {
				if _, err := q.MarkPlatformInvoicesPaid(ctx, db.MarkPlatformInvoicesPaidParams{
					TenantID: toPg(tenantID), SubscriptionID: pgtype.UUID{},
					AddonSlug: pgtype.Text{String: p.AddonSlug, Valid: true}}); err != nil {
					return err
				}
			}
			if p.AddonSlug == "" {
				// subscription-scoped payment: settle this sub's issued
				// invoices only — an addon payment must not sweep these.
				if _, err := q.MarkPlatformInvoicesPaid(ctx, db.MarkPlatformInvoicesPaidParams{
					TenantID: toPg(tenantID), SubscriptionID: sub.ID, AddonSlug: pgtype.Text{}}); err != nil {
					return err
				}
			}
			return nil
		}
		switch ev.EventType {
		case "invoice.paid", "payment.succeeded", "checkout.completed":
			if p.AddonSlug != "" {
				// pending addon settles: activate the grant + pay its
				// invoice, then AddonSubscribed flushes entitlements.
				n, err := q.ActivateTenantAddon(ctx, db.ActivateTenantAddonParams{
					TenantID: toPg(tenantID), AddonSlug: p.AddonSlug})
				if err != nil {
					return err
				}
				if n > 0 {
					if err := events.Emit(ctx, q, tenantID, uuid.Nil, "AddonSubscribed",
						"tenant_addon", tenantID, uuid.Nil, true,
						map[string]any{"tenant_id": tenantID, "addon_slug": p.AddonSlug, "via": "webhook"}); err != nil {
						return err
					}
				}
				if err := settle(); err != nil {
					return err
				}
				return mark()
			}
			if !sub.ID.Valid {
				return mark() // nothing to activate — ack
			}
			// Provider checkout carries the purchased plan — landing it
			// here is how upgrades arrive (self-serve only covers free).
			if p.PlanSlug != "" && p.PlanSlug != sub.PlanSlug {
				if _, err := q.GetPlan(ctx, p.PlanSlug); err == nil {
					if _, err := q.SetSubscriptionPlan(ctx, db.SetSubscriptionPlanParams{
						ID: sub.ID, PlanSlug: p.PlanSlug, ExpectedSlug: sub.PlanSlug}); err != nil {
						return err
					}
				}
			}
			if err := s.activate(ctx, q, &sub); err != nil {
				return err
			}
			if err := settle(); err != nil {
				return err
			}
		case "invoice.payment_failed", "payment.failed":
			// Failed charge only moves an active sub to past_due — trials
			// keep their window, lapsed states are already past the point,
			// cancelled is terminal.
			if sub.State != "active" {
				return mark()
			}
			graceEnd := time.Now().Add(time.Duration(s.settingInt(ctx, "billing.grace_period_days", 14)) * 24 * time.Hour)
			if err := s.transition(ctx, q, &sub, "past_due", graceEnd, time.Time{}, uuid.Nil); err != nil {
				return err
			}
		case "subscription.cancelled":
			if err := s.transition(ctx, q, &sub, "cancelled", time.Time{}, time.Now(), uuid.Nil); err != nil {
				return err
			}
		}
		return mark()
	})
}

// StaleWebhookEvents lists durable rows that recorded but never processed —
// the enqueue died after insert. The sweep re-drives them (Rule 9).
func (s *Service) StaleWebhookEvents(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.q.ListStaleWebhookEvents(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, uuid.UUID(r.Bytes))
	}
	return out, nil
}

// UsageRecomputeConsumer — §15: subscription changes (and provisioning)
// recompute countable usage meters for the tenant's current period
// (period_start = first of month). SetUsage semantics keep replays clean
// (Rule 27 — at-least-once safe).
func (s *Service) UsageRecomputeConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "SubscriptionChanged" && e.Type != "TenantProvisioned" {
		return nil
	}
	if e.TenantID == uuid.Nil {
		return nil
	}
	rows, err := q.TenantUsageCounts(ctx, toPg(e.TenantID))
	if err != nil {
		return err
	}
	period := time.Date(time.Now().Year(), time.Now().Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, r := range rows {
		if _, err := q.SetUsage(ctx, db.SetUsageParams{
			TenantID: toPg(e.TenantID), LimitKey: r.LimitKey,
			Used:        r.Used,
			PeriodStart: pgtype.Date{Time: period, Valid: true}}); err != nil {
			return err
		}
	}
	return nil
}

// ---------- add-ons (§07) ----------

// ListAddons is the operator's catalog list (all rows, incl. inactive).
func (s *Service) ListAddons(ctx context.Context) ([]db.AddonCatalog, error) {
	return s.q.ListAddonCatalog(ctx)
}

// UpsertAddon is the audited catalog admin path (platform.plans.manage).
func (s *Service) UpsertAddon(ctx context.Context, p db.UpsertAddonParams, actor uuid.UUID) (*db.AddonCatalog, error) {
	if p.AddonSlug == "" || p.Label == "" {
		return nil, ErrBadValue
	}
	if len(p.Grants) == 0 {
		p.Grants = json.RawMessage(`{}`)
	}
	if p.Currency == "" {
		p.Currency = "USD"
	}
	var row db.AddonCatalog
	err := s.withTx(ctx, func(q *db.Queries) error {
		r, err := q.UpsertAddon(ctx, p)
		if err != nil {
			return err
		}
		row = r
		ctxb, _ := json.Marshal(map[string]any{"addon_slug": p.AddonSlug})
		return q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
			ActorID: toPg(actor), Action: "addon.upsert",
			TargetType: pgtype.Text{String: "addon_catalog", Valid: true},
			Context:    ctxb})
	})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// TenantAddons is the tenant's own purchases + labels (GET /billing/addons).
func (s *Service) TenantAddons(ctx context.Context, tenantID uuid.UUID) ([]db.ListTenantAddonsRow, error) {
	return s.q.ListTenantAddons(ctx, toPg(tenantID))
}

// SubscribeAddon activates a catalog addon for the tenant — emits
// AddonSubscribed so the entitlement cache flushes (Rule 25). Eligibility:
// the addon must be active and the tenant's current plan must be in
// available_on_plans.
func (s *Service) SubscribeAddon(ctx context.Context, tenantID uuid.UUID, slug, term string, expiresAt *time.Time, actor uuid.UUID) (string, error) {
	if term == "" {
		term = "monthly"
	}
	if term != "monthly" && term != "annual" { // addons bill monthly|annual only
		return "", ErrBadBillingTerm
	}
	a, err := s.q.GetAddon(ctx, slug)
	if err != nil || !a.Active {
		return "", ErrPlanNotFound
	}
	plan := "starter"
	if sub, err := s.q.GetSubscription(ctx, toPg(tenantID)); err == nil {
		plan = sub.PlanSlug
		if sub.State == "restricted" || sub.State == "cancelled" {
			plan = "starter" // Rule 28 floor — same as entitlement resolve
		}
	}
	eligible := false
	for _, p := range a.AvailableOnPlans {
		if p == plan {
			eligible = true
			break
		}
	}
	if !eligible {
		return "", ErrAddonNotEligible
	}
	price := a.PriceMonthlyMinor.Int64
	if term == "annual" {
		price = a.PriceAnnualMinor.Int64
	}
	// Priced addons land 'pending' — they grant nothing until the provider
	// webhook settles the invoice (same Rule 9 self-serve contract as
	// subscriptions: a bare slug write never mints paid grants). Free
	// addons activate immediately with a zero paid invoice as the receipt.
	state := "pending"
	if price <= 0 {
		state = "active"
	}
	// expires_at is server-computed — client input is ignored so a caller
	// can't stretch a grant past its billing term. Pending checkouts lapse
	// at +grace (the sweep consumes 'pending' rows past expires_at); annual
	// grants run a year; monthly is perpetual until cancelled.
	var exp pgtype.Timestamptz
	switch {
	case state == "pending":
		exp = pgtype.Timestamptz{
			Time:  time.Now().Add(time.Duration(s.settingInt(ctx, "billing.grace_period_days", 14)) * 24 * time.Hour),
			Valid: true}
	case term == "annual":
		exp = pgtype.Timestamptz{Time: time.Now().Add(365 * 24 * time.Hour), Valid: true}
	}
	_ = expiresAt // client-supplied expiry intentionally ignored
	err = s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.SubscribeTenantAddon(ctx, db.SubscribeTenantAddonParams{
			TenantID: toPg(tenantID), AddonSlug: slug, State: state,
			BillingTerm: term, ExpiresAt: exp}); err != nil {
			return err
		}
		if err := s.issueInvoice(ctx, q, tenantID, uuid.Nil, slug, price, a.Currency); err != nil {
			return err
		}
		if state == "pending" {
			return nil // activation + AddonSubscribed land at settlement
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "AddonSubscribed",
			"tenant_addon", tenantID, actor, true,
			map[string]any{"tenant_id": tenantID, "addon_slug": slug, "term": term})
	})
	return state, err
}

// CancelAddon lapses a live addon — emits AddonCancelled (cache flush).
func (s *Service) CancelAddon(ctx context.Context, tenantID uuid.UUID, slug string, actor uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.CancelTenantAddon(ctx, db.CancelTenantAddonParams{
			TenantID: toPg(tenantID), AddonSlug: slug})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNoSubscription
		}
		// a pending addon's issued invoice is dead on cancel — void it so
		// the billing record matches (a paid invoice never matches 'issued').
		if err := q.VoidAddonIssuedInvoice(ctx, db.VoidAddonIssuedInvoiceParams{
			TenantID: toPg(tenantID), AddonSlug: pgtype.Text{String: slug, Valid: true}}); err != nil {
			return err
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "AddonCancelled",
			"tenant_addon", tenantID, actor, true,
			map[string]any{"tenant_id": tenantID, "addon_slug": slug})
	})
}

// ExpireAddonSweep lapses past-expiry addons — folded into SweepStates so
// the hourly billing tick emits AddonExpired (→ entitlement cache flush).
func (s *Service) ExpireAddonSweep(ctx context.Context) error {
	rows, err := s.q.ListExpiringAddons(ctx)
	if err != nil {
		return err
	}
	for _, r := range rows {
		tid := uuid.UUID(r.TenantID.Bytes)
		// Rule 12: the expiry flip and its event commit together — a
		// failed emit rolls the state back for the next sweep to retry.
		err := s.withTx(ctx, func(q *db.Queries) error {
			n, err := q.ExpireTenantAddon(ctx, r.ID)
			if err != nil {
				return err
			}
			if n == 0 {
				return nil // another path expired/cancelled it
			}
			return events.Emit(ctx, q, tid, uuid.Nil, "AddonExpired",
				"tenant_addon", tid, uuid.Nil, true,
				map[string]any{"tenant_id": tid, "addon_slug": r.AddonSlug})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// ---------- platform invoices (§15) ----------
// A durable invoice row documents every billable event — plan subscribe,
// plan change, addon subscribe. Zero-amount events (free/starter trials)
// record as paid; priced events record as issued until a provider webhook
// settles them.

func (s *Service) issueInvoice(ctx context.Context, q *db.Queries, tenantID, subID uuid.UUID, addonSlug string, amountMinor int64, currency string) error {
	seq, err := q.NextInvoiceNumber(ctx, toPg(tenantID))
	if err != nil {
		return err
	}
	state := "issued"
	if amountMinor == 0 {
		state = "paid" // nothing to collect — the record is the receipt
	}
	if currency == "" {
		currency = "USD"
	}
	_, err = q.CreatePlatformInvoice(ctx, db.CreatePlatformInvoiceParams{
		TenantID: toPg(tenantID), SubscriptionID: toPg(subID),
		AddonSlug:   pgtype.Text{String: addonSlug, Valid: addonSlug != ""},
		Number:      fmt.Sprintf("INV-%06d", seq),
		AmountMinor: amountMinor, Currency: currency, State: state,
	})
	return err
}

// ListInvoices — tenant billing history for GET /billing/invoices.
func (s *Service) ListInvoices(ctx context.Context, tenantID uuid.UUID, state string, lim, off int32) ([]db.PlatformInvoices, int64, error) {
	rows, err := s.q.ListPlatformInvoices(ctx, db.ListPlatformInvoicesParams{
		TenantID: toPg(tenantID), Column2: state, Limit: lim, Offset: off})
	if err != nil {
		return nil, 0, err
	}
	total, err := s.q.CountPlatformInvoices(ctx, db.CountPlatformInvoicesParams{
		TenantID: toPg(tenantID), Column2: state})
	return rows, total, err
}
