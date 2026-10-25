package billing

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
)

var testSeed = []byte("0123456789abcdef0123456789abcdef") // 32-byte ed25519 seed
var testSecret = []byte("wh-test-secret")

func testSvc(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	return NewService(pool, testSeed, testSecret), pool
}

func mkTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	r, err := db.New(pool).CreateTenant(context.Background(), db.CreateTenantParams{
		Name: "T-" + uuid.NewString()[:8], Slug: "t" + uuid.NewString()[:8], BillingEmail: "b@t.co",
	})
	if err != nil {
		t.Fatal(err)
	}
	return uuid.UUID(r.Bytes)
}

func webhookBody(t *testing.T, eventType string, tenantID uuid.UUID, id string) ([]byte, []byte) {
	t.Helper()
	body, err := json.Marshal(map[string]string{"id": id, "type": eventType, "tenant_id": tenantID.String()})
	if err != nil {
		t.Fatal(err)
	}
	m := hmac.New(sha256.New, testSecret)
	m.Write(body)
	return body, m.Sum(nil)
}

func TestLicenseSignRoundtrip(t *testing.T) {
	svc := NewService(nil, testSeed, nil)
	payload := svc.licensePayload("dep-1", time.Now().Add(time.Hour), []byte(`{"capabilities":["x"]}`))
	sig := svc.sign(payload)
	pub := svc.signKey.Public().(ed25519.PublicKey)
	if !ed25519.Verify(pub, payload, sig) {
		t.Fatal("signature should verify against derived public key")
	}
	if ed25519.Verify(pub, append(payload, 'x'), sig) {
		t.Fatal("tampered payload must not verify")
	}
}

func TestCreateSubscriptionGates(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)

	if _, err := svc.CreateSubscription(ctx, tenant, "basic", "monthly", ""); err != ErrPaymentRequired {
		t.Fatalf("paid plan should need checkout, got %v", err)
	}
	if _, err := svc.CreateSubscription(ctx, tenant, "enterprise", "monthly", ""); err != ErrPaymentRequired {
		t.Fatalf("unlisted/contract plan should need checkout, got %v", err)
	}
	if _, err := svc.CreateSubscription(ctx, tenant, "nope", "monthly", ""); err != ErrPlanNotFound {
		t.Fatalf("unknown plan, got %v", err)
	}
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "weekly", ""); err != ErrBadBillingTerm {
		t.Fatalf("bad term, got %v", err)
	}
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatalf("free plan should create, got %v", err)
	}
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != ErrSubscriptionExists {
		t.Fatalf("second open sub should conflict, got %v", err)
	}
	// resubscribe path: a cancelled history doesn't block a fresh free sub
	if err := svc.Cancel(ctx, tenant, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatalf("resubscribe after cancel should work, got %v", err)
	}
}

func TestChangePlanGates(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)

	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangePlan(ctx, tenant, "basic", uuid.Nil); err != ErrPaymentRequired {
		t.Fatalf("upgrade should need payment, got %v", err)
	}
	// seat the tenant on professional directly (provider/platform flow)
	if _, err := pool.Exec(ctx,
		"UPDATE subscriptions SET plan_slug='professional', state='active' WHERE tenant_id=$1",
		tenant); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangePlan(ctx, tenant, "basic", uuid.Nil); err != nil {
		t.Fatalf("downgrade pro→basic should be allowed, got %v", err)
	}
	sub, _ := svc.GetSubscription(ctx, tenant)
	if sub.PlanSlug != "basic" {
		t.Fatalf("expected basic, got %s", sub.PlanSlug)
	}
	if err := svc.Cancel(ctx, tenant, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangePlan(ctx, tenant, "starter", uuid.Nil); err != ErrBadTransition {
		t.Fatalf("cancelled sub can't change plan, got %v", err)
	}
}

func TestWebhookLifecycle(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatal(err)
	}

	// bad signature rejected before any record
	if _, err := svc.RecordWebhook(ctx, "testprov", []byte(`{"id":"e0","type":"invoice.paid"}`), []byte("bad")); err != ErrBadSignature {
		t.Fatalf("bad sig should fail, got %v", err)
	}

	body, sig := webhookBody(t, "invoice.paid", tenant, "evt-"+uuid.NewString()[:12])
	rawID, err := svc.RecordWebhook(ctx, "testprov", body, sig)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := svc.RecordWebhook(ctx, "testprov", body, sig); err != ErrDuplicateEvent {
		t.Fatalf("replay should dedupe, got %v", err)
	}
	if err := svc.ProcessWebhook(ctx, rawID); err != nil {
		t.Fatalf("process: %v", err)
	}
	sub, _ := svc.GetSubscription(ctx, tenant)
	if sub.State != "active" || !sub.CurrentPeriodEnd.Valid {
		t.Fatalf("expected active with period end, got %s", sub.State)
	}
	if err := svc.ProcessWebhook(ctx, rawID); err != nil {
		t.Fatal("reprocess must be a no-op")
	}

	// silent provider: lapse active past period_end → past_due → restricted
	if _, err := pool.Exec(ctx,
		"UPDATE subscriptions SET current_period_end = now() - interval '1h' WHERE tenant_id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepStates(ctx); err != nil {
		t.Fatal(err)
	}
	sub, _ = svc.GetSubscription(ctx, tenant)
	if sub.State != "past_due" {
		t.Fatalf("lapsed active should be past_due, got %s", sub.State)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE subscriptions SET grace_ends_at = now() - interval '1h' WHERE tenant_id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepStates(ctx); err != nil {
		t.Fatal(err)
	}
	sub, _ = svc.GetSubscription(ctx, tenant)
	if sub.State != "restricted" {
		t.Fatalf("expired grace should restrict, got %s", sub.State)
	}
}

func TestTrialSweep(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE subscriptions SET trial_ends_at = now() - interval '1h' WHERE tenant_id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepStates(ctx); err != nil {
		t.Fatal(err)
	}
	sub, _ := svc.GetSubscription(ctx, tenant)
	if sub.State != "grace" {
		t.Fatalf("expired trial should enter grace, got %s", sub.State)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE subscriptions SET grace_ends_at = now() - interval '1h' WHERE tenant_id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	if err := svc.SweepStates(ctx); err != nil {
		t.Fatal(err)
	}
	sub, _ = svc.GetSubscription(ctx, tenant)
	if sub.State != "restricted" {
		t.Fatalf("expired grace should restrict, got %s", sub.State)
	}
}

func TestLicenseIssueValidateRenew(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, svc.pool)
	depID := "dep-" + uuid.NewString()[:12]
	ents := json.RawMessage(`{"capabilities":["app.finance_lite"],"limits":{"limit.branches":5}}`)

	if _, err := svc.IssueLicense(ctx, uuid.New(), depID, "self_hosted", ents, time.Now().Add(time.Hour), uuid.Nil); err != ErrTenantNotFound {
		t.Fatalf("bad tenant should 404, got %v", err)
	}
	if _, err := svc.IssueLicense(ctx, tenant, depID, "self_hosted", ents, time.Now().Add(-time.Hour), uuid.Nil); err != ErrBadExpiry {
		t.Fatalf("past expiry should fail, got %v", err)
	}
	exp := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	if _, err := svc.IssueLicense(ctx, tenant, depID, "self_hosted", ents, exp, uuid.Nil); err != nil {
		t.Fatalf("issue: %v", err)
	}

	row, err := svc.q.GetLicenseByDeployment(ctx, depID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ValidateLicense(ctx, depID, hex.EncodeToString(row.Signature)); err != nil {
		t.Fatalf("valid license should verify, got %v", err)
	}
	if _, err := svc.ValidateLicense(ctx, depID, hex.EncodeToString(make([]byte, 64))); err != ErrBadSignature {
		t.Fatalf("forged sig should fail, got %v", err)
	}
	if _, err := svc.ValidateLicense(ctx, "dep-missing-"+uuid.NewString()[:8], "00"); err != ErrLicenseNotFound {
		t.Fatalf("unknown deployment, got %v", err)
	}
	// renewal on the same deployment_id rotates in place, not a conflict
	exp2 := time.Now().Add(48 * time.Hour).Truncate(time.Second)
	if _, err := svc.IssueLicense(ctx, tenant, depID, "self_hosted", ents, exp2, uuid.Nil); err != nil {
		t.Fatalf("renewal should upsert, got %v", err)
	}
	if _, err := svc.ValidateLicense(ctx, depID, hex.EncodeToString(row.Signature)); err != ErrBadSignature {
		t.Fatalf("pre-renewal signature must not verify after rotation, got %v", err)
	}
}

// Compare-and-set: a stale read can't regress a subscription that already
// moved on — the transition is a silent no-op (no event, no state change).
func TestTransitionCAS(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatal(err)
	}
	stale, err := svc.GetSubscription(ctx, tenant) // reads state='trial'
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE subscriptions SET state='active' WHERE tenant_id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	// a payment.failed arriving after the sub already activated must not
	// drag it back to past_due
	q := db.New(pool)
	if err := svc.transition(ctx, q, stale, "past_due", time.Now(), time.Time{}, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	cur, _ := svc.GetSubscription(ctx, tenant)
	if cur.State != "active" {
		t.Fatalf("stale transition regressed state to %s", cur.State)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM domain_events WHERE event_type='SubscriptionChanged' AND payload->>'to'='past_due' AND tenant_id=$1`,
		tenant).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("CAS no-op emitted a phantom SubscriptionChanged")
	}
}

// Terminal/lapsed-state guards: a late invoice.paid can't resurrect a
// cancelled subscription, and payment.failed only lapses an active one.
func TestWebhookStateGuards(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatal(err)
	}
	// payment.failed on trial → trial keeps its window (no past_due skip)
	body, sig := webhookBody(t, "payment.failed", tenant, "evt-"+uuid.NewString()[:12])
	rawID, err := svc.RecordWebhook(ctx, "testprov", body, sig)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessWebhook(ctx, rawID); err != nil {
		t.Fatal(err)
	}
	if sub, _ := svc.GetSubscription(ctx, tenant); sub.State != "trial" {
		t.Fatalf("failed charge on trial should be a no-op, got %s", sub.State)
	}
	// cancelled + late invoice.paid → stays cancelled (terminal)
	if err := svc.Cancel(ctx, tenant, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	body, sig = webhookBody(t, "invoice.paid", tenant, "evt-"+uuid.NewString()[:12])
	rawID, err = svc.RecordWebhook(ctx, "testprov", body, sig)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessWebhook(ctx, rawID); err != nil {
		t.Fatal(err)
	}
	if sub, _ := svc.GetSubscription(ctx, tenant); sub.State != "cancelled" {
		t.Fatalf("late payment resurrected a cancelled sub to %s", sub.State)
	}
}

// An unpriced/unlisted current plan can't prove a paid target is a
// downgrade — self-serve demands payment instead of a bare slug write.
func TestChangePlanUnpricedCurrent(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE subscriptions SET plan_slug='enterprise', state='active' WHERE tenant_id=$1", tenant); err != nil {
		t.Fatal(err)
	}
	if err := svc.ChangePlan(ctx, tenant, "basic", uuid.Nil); err != ErrPaymentRequired {
		t.Fatalf("enterprise→basic paid move should need payment, got %v", err)
	}
	if err := svc.ChangePlan(ctx, tenant, "starter", uuid.Nil); err != nil {
		t.Fatalf("free-plan move should still be allowed, got %v", err)
	}
}

func TestStaleWebhookRedrive(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tenant := mkTenant(t, pool)
	if _, err := svc.CreateSubscription(ctx, tenant, "starter", "monthly", ""); err != nil {
		t.Fatal(err)
	}
	body, sig := webhookBody(t, "subscription.cancelled", tenant, "evt-"+uuid.NewString()[:12])
	rawID, err := svc.RecordWebhook(ctx, "testprov", body, sig)
	if err != nil {
		t.Fatal(err)
	}
	// fresh unprocessed rows are not "stale" — the worker may be mid-flight
	if ids, _ := svc.StaleWebhookEvents(ctx); len(ids) != 0 {
		t.Fatalf("fresh event shouldn't be stale, got %v", ids)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE billing_webhook_events SET created_at = now() - interval '10m' WHERE id=$1", rawID); err != nil {
		t.Fatal(err)
	}
	ids, err := svc.StaleWebhookEvents(ctx)
	if err != nil || len(ids) != 1 {
		t.Fatalf("expected 1 stale event, got %v %v", ids, err)
	}
	if err := svc.ProcessWebhook(ctx, rawID); err != nil {
		t.Fatal(err)
	}
	if ids, _ := svc.StaleWebhookEvents(ctx); len(ids) != 0 {
		t.Fatal("processed event must leave the stale set")
	}
	sub, _ := svc.GetSubscription(ctx, tenant)
	if sub.State != "cancelled" {
		t.Fatalf("expected cancelled, got %s", sub.State)
	}
}
