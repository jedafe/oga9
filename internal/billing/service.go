// Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
// Subscriptions + lifecycle machine, self-hosted licenses (Rule 29), and
// Rule 9 billing webhooks (durable record → async processing). Every
// transition emits SubscriptionChanged in the same transaction.
package billing

import (
	"context"
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
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
)

var (
	ErrNoSubscription      = errors.New("subscription_not_found")
	ErrPlanNotFound        = errors.New("plan_not_found")
	ErrBadBillingTerm      = errors.New("invalid_billing_term")
	ErrBadTransition       = errors.New("invalid_state_transition")
	ErrDuplicateEvent      = errors.New("duplicate_webhook_event")
	ErrBadSignature        = errors.New("invalid_signature")
	ErrLicenseNotFound     = errors.New("license_not_found")
	ErrLicenseExpired      = errors.New("license_expired")
	ErrWebhookUnconfigured = errors.New("webhook_provider_not_configured")
)

var billingTerms = map[string]bool{"monthly": true, "annual": true, "enterprise_contract": true}

type Service struct {
	pool          *pgxpool.Pool
	q             *db.Queries
	signKey       []byte // OGA9_LICENSE_SIGNING_KEY
	webhookSecret []byte // OGA9_BILLING_WEBHOOK_SECRET
}

func NewService(pool *pgxpool.Pool, vk valkey.Client, signKey, webhookSecret []byte) *Service {
	return &Service{pool: pool, q: db.New(pool), signKey: signKey, webhookSecret: webhookSecret}
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

// CreateSubscription starts a plan on trial (billing.trial_days, Rule 3) —
// used both at tenant provision and for POST /billing/subscription on a
// tenant with no current subscription.
func (s *Service) CreateSubscription(ctx context.Context, tenantID uuid.UUID, plan, term, actorID string) (uuid.UUID, error) {
	if ok, _ := s.q.PlanExists(ctx, plan); !ok {
		return uuid.Nil, ErrPlanNotFound
	}
	if !billingTerms[term] {
		return uuid.Nil, ErrBadBillingTerm
	}
	trialDays := s.settingInt(ctx, "billing.trial_days", 14)
	trialEnd := time.Now().Add(time.Duration(trialDays) * 24 * time.Hour)
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		sid, err := q.CreateSubscription(ctx, db.CreateSubscriptionParams{
			TenantID: toPg(tenantID), PlanSlug: plan, State: "trial",
			BillingTerm: term, TrialEndsAt: pgTime(trialEnd),
		})
		if err != nil {
			return err
		}
		id = uuid.UUID(sid.Bytes)
		uid := uuid.Nil
		if u, err := uuid.Parse(actorID); err == nil {
			uid = u
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
func (s *Service) transition(ctx context.Context, q *db.Queries, sub *db.Subscriptions, to string, graceEnd, cancelledAt time.Time, actor uuid.UUID) error {
	from := sub.State
	if from == to {
		return nil
	}
	if err := q.SetSubscriptionState(ctx, db.SetSubscriptionStateParams{
		ID: sub.ID, State: to, GraceEndsAt: pgTime(graceEnd), CancelledAt: pgTime(cancelledAt),
	}); err != nil {
		return err
	}
	return events.Emit(ctx, q, uuid.UUID(sub.TenantID.Bytes), uuid.Nil, "SubscriptionChanged",
		"subscription", uuid.UUID(sub.ID.Bytes), actor, true,
		map[string]any{"tenant_id": uuid.UUID(sub.TenantID.Bytes), "from": from, "to": to, "plan_slug": sub.PlanSlug})
}

// ChangePlan swaps the plan slug (proration is a §15 concern); emits
// SubscriptionChanged with the plan change recorded.
func (s *Service) ChangePlan(ctx context.Context, tenantID uuid.UUID, plan string, actor uuid.UUID) error {
	sub, err := s.GetSubscription(ctx, tenantID)
	if err != nil {
		return err
	}
	if ok, _ := s.q.PlanExists(ctx, plan); !ok {
		return ErrPlanNotFound
	}
	if sub.PlanSlug == plan {
		return nil
	}
	if sub.State == "cancelled" {
		return ErrBadTransition // cancelled subs can't change plan — resubscribe
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.SetSubscriptionPlan(ctx, db.SetSubscriptionPlanParams{ID: sub.ID, PlanSlug: plan}); err != nil {
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
		return s.transition(ctx, q, sub, "cancelled", time.Time{}, time.Now(), actor)
	})
}

// activate marks the subscription active (webhook: payment succeeded) and
// extends the period end.
func (s *Service) activate(ctx context.Context, q *db.Queries, sub *db.Subscriptions) error {
	period := 30 * 24 * time.Hour
	if sub.BillingTerm == "annual" {
		period = 365 * 24 * time.Hour
	}
	if err := q.SetSubscriptionState(ctx, db.SetSubscriptionStateParams{
		ID: sub.ID, State: "active", CurrentPeriodEnd: pgTime(time.Now().Add(period)),
	}); err != nil {
		return err
	}
	return events.Emit(ctx, q, uuid.UUID(sub.TenantID.Bytes), uuid.Nil, "SubscriptionChanged",
		"subscription", uuid.UUID(sub.ID.Bytes), uuid.Nil, true,
		map[string]any{"tenant_id": uuid.UUID(sub.TenantID.Bytes), "from": sub.State, "to": "active", "plan_slug": sub.PlanSlug})
}

// SweepStates drives the time-based lifecycle transitions (§07):
//
//	trial expired      → grace (fresh grace window from billing.grace_period_days)
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
		case "past_due", "grace":
			to = "restricted"
		default:
			continue
		}
		err := s.withTx(ctx, func(q *db.Queries) error {
			return s.transition(ctx, q, &sub, to, graceEnd, time.Time{}, uuid.Nil)
		})
		if err != nil {
			log.Printf("subscription sweep: %s %s→%s failed: %v", r.ID.Bytes, r.State, to, err)
		}
	}
	return nil
}

// ---------- licenses (self-hosted, Rule 29) ----------

func (s *Service) licensePayload(deploymentID string, expiresAt time.Time, entitlements []byte) []byte {
	return []byte(fmt.Sprintf("%s|%s|%s", deploymentID, expiresAt.UTC().Format(time.RFC3339), string(entitlements)))
}

func (s *Service) sign(payload []byte) []byte {
	m := hmac.New(sha256.New, s.signKey)
	m.Write(payload)
	return m.Sum(nil)
}

// IssueLicense signs + stores a self-hosted license (control-plane action,
// platform.licenses.issue). Offline operation: deployments validate the
// signed file locally (Rule 29).
func (s *Service) IssueLicense(ctx context.Context, tenantID uuid.UUID, deploymentID, edition string, entitlements json.RawMessage, expiresAt time.Time, actor uuid.UUID) (uuid.UUID, error) {
	if !json.Valid(entitlements) {
		return uuid.Nil, errors.New("invalid_entitlements")
	}
	sig := s.sign(s.licensePayload(deploymentID, expiresAt, entitlements))
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		lid, err := q.InsertLicense(ctx, db.InsertLicenseParams{
			TenantID: toPg(tenantID), DeploymentID: deploymentID, Edition: edition,
			Entitlements: entitlements, ExpiresAt: pgTime(expiresAt), Signature: sig,
		})
		if err != nil {
			return err
		}
		id = uuid.UUID(lid.Bytes)
		return events.Emit(ctx, q, tenantID, uuid.Nil, "LicenseIssued", "license", id, actor, true,
			map[string]any{"license_id": id, "deployment_id": deploymentID})
	})
	return id, err
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
	want := s.sign(s.licensePayload(deploymentID, row.ExpiresAt.Time, row.Entitlements))
	got, err := hex.DecodeString(signatureHex)
	if err != nil || !hmac.Equal(got, want) {
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
		return uuid.Nil, errors.New("invalid_webhook_payload")
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
// already-processed events skip; unknown types are acknowledged.
func (s *Service) ProcessWebhook(ctx context.Context, rawEventID uuid.UUID) error {
	ev, err := s.q.GetWebhookEvent(ctx, toPg(rawEventID))
	if err != nil {
		return err
	}
	if ev.ProcessedAt.Valid {
		return nil
	}
	var p struct {
		TenantID string `json:"tenant_id"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	tenantID, err := uuid.Parse(p.TenantID)
	if err != nil {
		return s.markProcessed(ctx, rawEventID) // unrouteable — ack, don't retry forever
	}
	sub, err := s.GetSubscription(ctx, tenantID)
	if err != nil {
		return s.markProcessed(ctx, rawEventID)
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		switch ev.EventType {
		case "invoice.paid", "payment.succeeded", "checkout.completed":
			if err := s.activate(ctx, q, sub); err != nil {
				return err
			}
		case "invoice.payment_failed", "payment.failed":
			graceEnd := time.Now().Add(time.Duration(s.settingInt(ctx, "billing.grace_period_days", 14)) * 24 * time.Hour)
			if err := s.transition(ctx, q, sub, "past_due", graceEnd, time.Time{}, uuid.Nil); err != nil {
				return err
			}
		case "subscription.cancelled":
			if err := s.transition(ctx, q, sub, "cancelled", time.Time{}, time.Now(), uuid.Nil); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return s.markProcessed(ctx, rawEventID)
}

func (s *Service) markProcessed(ctx context.Context, rawEventID uuid.UUID) error {
	_, err := s.q.MarkWebhookProcessed(ctx, toPg(rawEventID))
	return err
}
