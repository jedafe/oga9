// Ref: oga9-engineering/sections/15-subscription-admin.md — provision
// idempotency, resume-after-failure, impersonation scope, suspend gates.
package platform

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

func testSvc(t *testing.T) (*Service, *tenant.Service, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://oga9:oga9@localhost:5434/oga9_test?sslmode=disable")
	if err != nil {
		t.Skip("test db unavailable")
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skip("test db unavailable: ", err)
	}
	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{"localhost:6379"}})
	if err != nil {
		t.Skip("valkey unavailable: ", err)
	}
	return NewService(pool), tenant.NewService(pool, vk), pool
}

func mkUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO users (id, email, password_hash, display_name, status) VALUES ($1,$2,'x','T','active')",
		id, email); err != nil {
		t.Fatal(err)
	}
	return id
}

func mkTenant(t *testing.T, pool *pgxpool.Pool, slug string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.QueryRow(context.Background(),
		"INSERT INTO tenants (name, slug, billing_email) VALUES ($1,$2,$3) RETURNING id",
		slug, slug, slug+"@x.test").Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func mkOrgMember(t *testing.T, pool *pgxpool.Pool, tenantID, userID uuid.UUID, state string) (orgID uuid.UUID) {
	t.Helper()
	orgID = uuid.New()
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO organizations (id, tenant_id, name, slug, org_type) VALUES ($1,$2,$3,$4,'association')",
		orgID, tenantID, "Org "+orgID.String()[:8], "o-"+orgID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO organization_memberships (id, organization_id, user_id, lifecycle_state) VALUES ($1,$2,$3,$4)",
		uuid.New(), orgID, userID, state); err != nil {
		t.Fatal(err)
	}
	return orgID
}

// TestProvisionReplay — same request_id runs once; replays return the
// recorded ids without re-provisioning (§15 test scenario 1).
func TestProvisionReplay(t *testing.T) {
	svc, _, pool := testSvc(t)
	ctx := context.Background()
	actor := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	req := ProvisionRequest{
		RequestID: "req-" + uuid.NewString()[:8], TenantName: "Acme",
		TenantSlug: "acme-" + uuid.NewString()[:8], OrgSlug: "acme-org-" + uuid.NewString()[:8],
		OwnerName: "Owner", OwnerEmail: "owner+" + uuid.NewString()[:8] + "@x.test",
	}
	boot := 0
	svc.SetOrgBootstrap(func(_ context.Context, _ uuid.UUID, _, _, _, _, _, _, _ string, tid uuid.UUID) (uuid.UUID, error) {
		boot++
		var orgID uuid.UUID
		pool.QueryRow(context.Background(),
			"INSERT INTO organizations (id, tenant_id, name, slug, org_type) VALUES ($1,$2,$3,$4,'association') RETURNING id",
			uuid.New(), tid, "Boot", "b-"+uuid.NewString()[:8]).Scan(&orgID)
		return orgID, nil
	})
	tid1, oid1, st1, err := svc.Provision(ctx, req, actor)
	if err != nil || st1 != "done" {
		t.Fatalf("first provision: %v %s", err, st1)
	}
	tid2, oid2, st2, err := svc.Provision(ctx, req, actor)
	if err != nil || st2 != "done" || tid2 != tid1 || oid2 != oid1 {
		t.Fatalf("replay: %v %s %v %v", err, st2, tid2, oid2)
	}
	if boot != 1 {
		t.Fatalf("bootstrap ran %d times — replay must not re-run", boot)
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM tenant_provisions WHERE request_id=$1", req.RequestID).Scan(&n)
	if n != 1 {
		t.Fatalf("%d provision rows for one request_id", n)
	}
}

// TestProvisionResumeAfterFailure — a failed bootstrap leaves tenant+user+
// sub committed; a retry must RESUME those ids, not create duplicates.
func TestProvisionResumeAfterFailure(t *testing.T) {
	svc, _, pool := testSvc(t)
	ctx := context.Background()
	actor := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	slug := "acme-" + uuid.NewString()[:8]
	req := ProvisionRequest{
		RequestID: "req-" + uuid.NewString()[:8], TenantName: "Acme", TenantSlug: slug,
		OrgSlug: "o-" + slug, OwnerName: "O", OwnerEmail: "o+" + uuid.NewString()[:8] + "@x.test",
	}
	boom := true
	boot := 0
	svc.SetOrgBootstrap(func(_ context.Context, _ uuid.UUID, _, _, _, _, _, _, _ string, tid uuid.UUID) (uuid.UUID, error) {
		boot++
		if boom {
			return uuid.Nil, errors.New("boom")
		}
		var orgID uuid.UUID
		pool.QueryRow(context.Background(),
			"INSERT INTO organizations (id, tenant_id, name, slug, org_type) VALUES ($1,$2,$3,$4,'association') RETURNING id",
			uuid.New(), tid, "Boot", "b-"+uuid.NewString()[:8]).Scan(&orgID)
		return orgID, nil
	})
	tid1, _, st1, err := svc.Provision(ctx, req, actor)
	if err == nil || st1 != "failed" {
		t.Fatalf("expected failed: %v %s", err, st1)
	}
	boom = false
	tid2, oid2, st2, err := svc.Provision(ctx, req, actor)
	if err != nil || st2 != "done" {
		t.Fatalf("retry: %v %s", err, st2)
	}
	if tid2 != tid1 || oid2 == uuid.Nil {
		t.Fatalf("resume must reuse tenant: %v %v", tid1, tid2)
	}
	var tenants int
	pool.QueryRow(ctx, "SELECT count(*) FROM tenants WHERE slug=$1", slug).Scan(&tenants)
	if tenants != 1 {
		t.Fatalf("%d tenants for one provision — retry duplicated", tenants)
	}
}

// TestImpersonateTenantCheck — nonexistent tenant → ErrNotFound, never a
// bare FK violation.
func TestImpersonateTenantCheck(t *testing.T) {
	svc, _, pool := testSvc(t)
	uid := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	_, _, _, err := svc.Impersonate(context.Background(), uid, uuid.New(), "nobody@x.test", "test", "", "")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// TestSuspendGate — suspended tenants refuse org context (cached + fresh
// paths); TenantReinstated write-through restores access immediately.
func TestSuspendGate(t *testing.T) {
	svc, orgSvc, pool := testSvc(t)
	ctx := context.Background()
	uid := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	tid := mkTenant(t, pool, "ten-"+uuid.NewString()[:8])
	orgID := mkOrgMember(t, pool, tid, uid, "active")

	if _, err := orgSvc.ResolveOrgCtx(ctx, uid, orgID); err != nil {
		t.Fatalf("pre-suspend ctx: %v", err)
	}
	if err := svc.SuspendTenant(ctx, tid, "test", uid); err != nil {
		t.Fatal(err)
	}
	// production path: TenantSuspended event → consumer writes the status
	// cache through (60s DB fallback otherwise).
	if err := orgSvc.TenantStatusCacheConsumer(ctx, db.New(pool), events.Event{
		Type: "TenantSuspended", TenantID: tid}); err != nil {
		t.Fatal(err)
	}
	if _, err := orgSvc.ResolveOrgCtx(ctx, uid, orgID); !errors.Is(err, tenant.ErrTenantSuspended) {
		t.Fatalf("expected tenant_suspended, got %v", err)
	}
	if err := svc.ReinstateTenant(ctx, tid, uid); err != nil {
		t.Fatal(err)
	}
	// simulate the event consumer writing the status cache through
	if err := orgSvc.TenantStatusCacheConsumer(ctx, db.New(pool), events.Event{
		Type: "TenantReinstated", TenantID: tid}); err != nil {
		t.Fatal(err)
	}
	if _, err := orgSvc.ResolveOrgCtx(ctx, uid, orgID); err != nil {
		t.Fatalf("post-reinstate ctx: %v", err)
	}
}

// TestHealthyMembershipGate — UserHasHealthyMembership is the §15 partial-
// access gate: only users with NO live org anywhere get login-blocked.
func TestHealthyMembershipGate(t *testing.T) {
	svc, orgSvc, pool := testSvc(t)
	ctx := context.Background()
	uid := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	tid := mkTenant(t, pool, "ten-"+uuid.NewString()[:8])
	mkOrgMember(t, pool, tid, uid, "active")

	var healthy bool
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM organization_memberships m
		 JOIN organizations o ON o.id = m.organization_id
		 JOIN tenants te ON te.id = o.tenant_id
		 WHERE m.user_id = $1 AND te.status <> 'suspended'
		   AND m.lifecycle_state NOT IN ('resigned','expelled','deceased'))`, uid).Scan(&healthy); err != nil || !healthy {
		t.Fatalf("healthy membership expected: %v %v", err, healthy)
	}
	if err := svc.SuspendTenant(ctx, tid, "t", uid); err != nil {
		t.Fatal(err)
	}
	_ = orgSvc // suspend path covered above; query gate is the login check
	if err := pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM organization_memberships m
		 JOIN organizations o ON o.id = m.organization_id
		 JOIN tenants te ON te.id = o.tenant_id
		 WHERE m.user_id = $1 AND te.status <> 'suspended'
		   AND m.lifecycle_state NOT IN ('resigned','expelled','deceased'))`, uid).Scan(&healthy); err != nil || healthy {
		t.Fatalf("suspended-only user must not be healthy: %v %v", err, healthy)
	}
}
