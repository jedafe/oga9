package entitlements

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
)

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
	addr := os.Getenv("OGA9_TEST_VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}})
	if err != nil {
		t.Fatal(err)
	}
	return NewService(pool, vk), pool
}

func mkTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	r, err := db.New(pool).CreateTenant(context.Background(), db.CreateTenantParams{
		Name: "E-" + uuid.NewString()[:8], Slug: "e" + uuid.NewString()[:8], BillingEmail: "b@t.co",
	})
	if err != nil {
		t.Fatal(err)
	}
	return uuid.UUID(r.Bytes)
}

func mkSub(t *testing.T, pool *pgxpool.Pool, tenantID uuid.UUID, plan, state string) {
	t.Helper()
	_, err := db.New(pool).CreateSubscription(context.Background(), db.CreateSubscriptionParams{
		TenantID: pgtype.UUID{Bytes: tenantID, Valid: true}, PlanSlug: plan,
		State: state, BillingTerm: "monthly",
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestEntitlementResolution(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()

	// no subscription → starter floor
	free := mkTenant(t, pool)
	if !svc.IsEnabled(ctx, free, "app.members") || svc.IsEnabled(ctx, free, "app.finance_lite") {
		t.Fatal("starter floor should grant members, not finance_lite")
	}
	if got := svc.Limit(ctx, free, "limit.members.active"); got != 50 {
		t.Fatalf("starter member limit = 50, got %d", got)
	}

	// active professional → bundled paid capabilities
	pro := mkTenant(t, pool)
	mkSub(t, pool, pro, "professional", "active")
	if !svc.IsEnabled(ctx, pro, "app.finance_lite") {
		t.Fatal("professional bundles finance_lite")
	}
	if got := svc.Limit(ctx, pro, "limit.members.active"); got != 5000 {
		t.Fatalf("pro member limit = 5000, got %d", got)
	}

	// Rule 28: restricted/cancelled drop to the starter floor — records stay,
	// paid capabilities stop resolving
	for _, state := range []string{"restricted", "cancelled"} {
		tid := mkTenant(t, pool)
		mkSub(t, pool, tid, "professional", state)
		if svc.IsEnabled(ctx, tid, "app.finance_lite") {
			t.Fatalf("%s tenant must not resolve paid capabilities", state)
		}
		if got := svc.Limit(ctx, tid, "limit.members.active"); got != 50 {
			t.Fatalf("%s should floor at starter limits, got %d", state, got)
		}
	}

	// license merge: unexpired self-hosted license unions in (Rule 29)
	lic := mkTenant(t, pool)
	mkSub(t, pool, lic, "starter", "active")
	ents, _ := json.Marshal(map[string]any{
		"capabilities": []string{"app.finance_lite"},
		"limits":       map[string]int64{"limit.branches": 99},
	})
	if _, err := db.New(pool).InsertLicense(ctx, db.InsertLicenseParams{
		TenantID: pgtype.UUID{Bytes: lic, Valid: true}, DeploymentID: "dep-" + uuid.NewString()[:8],
		Edition: "self_hosted", Entitlements: ents,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(time.Hour), Valid: true},
		Signature: []byte("x"),
	}); err != nil {
		t.Fatal(err)
	}
	if !svc.IsEnabled(ctx, lic, "app.finance_lite") {
		t.Fatal("license capability should merge in")
	}
	if got := svc.Limit(ctx, lic, "limit.branches"); got != 99 {
		t.Fatalf("license limit takes the greater grant, got %d", got)
	}

	// expired license must not merge
	expT := mkTenant(t, pool)
	if _, err := db.New(pool).InsertLicense(ctx, db.InsertLicenseParams{
		TenantID: pgtype.UUID{Bytes: expT, Valid: true}, DeploymentID: "dep-" + uuid.NewString()[:8],
		Edition: "self_hosted", Entitlements: ents,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(-time.Hour), Valid: true},
		Signature: []byte("x"),
	}); err != nil {
		t.Fatal(err)
	}
	if svc.IsEnabled(ctx, expT, "app.finance_lite") {
		t.Fatal("expired license must not grant capabilities")
	}
}

func TestCheckLimitThresholds(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	tid := mkTenant(t, pool) // starter floor: limit.members.active=50

	if ok, warn := svc.CheckLimit(ctx, tid, "limit.members.active", 10); !ok || warn {
		t.Fatal("10/50 should allow without warning")
	}
	if ok, warn := svc.CheckLimit(ctx, tid, "limit.members.active", 45); !ok || !warn {
		t.Fatal("45/50 is ≥80% — warn expected")
	}
	if ok, _ := svc.CheckLimit(ctx, tid, "limit.members.active", 50); ok {
		t.Fatal("at limit → deny")
	}
	// unconfigured limit key → deny-by-default
	if ok, _ := svc.CheckLimit(ctx, tid, "limit.nonexistent", 0); ok {
		t.Fatal("missing limit should deny")
	}
}
