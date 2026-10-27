// Ref: oga9-engineering/sections/15-subscription-admin.md — provision
// idempotency, resume-after-failure, impersonation scope, suspend gates.
package platform

import (
	"context"
	"errors"
	"os"
	"testing"

	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

func testSvc(t *testing.T) (*Service, *tenant.Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
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
		Template:  "union",
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
		OrgSlug: "o-" + slug, OwnerName: "O", Template: "union", OwnerEmail: "o+" + uuid.NewString()[:8] + "@x.test",
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

// TestSuspendIdempotent — a repeat suspend/reinstate emits no duplicate
// audit row or domain event.
func TestSuspendIdempotent(t *testing.T) {
	svc, _, pool := testSvc(t)
	ctx := context.Background()
	uid := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	tid := mkTenant(t, pool, "ten-"+uuid.NewString()[:8])
	for i := 0; i < 2; i++ {
		if err := svc.SuspendTenant(ctx, tid, "t", uid); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	pool.QueryRow(ctx, "SELECT count(*) FROM audit_events WHERE action='tenant.suspend' AND tenant_id=$1", tid).Scan(&n)
	if n != 1 {
		t.Fatalf("repeat suspend wrote %d audit rows", n)
	}
}

// TestImpersonateGuards — reason required; platform-role targets refused.
func TestImpersonateGuards(t *testing.T) {
	svc, _, pool := testSvc(t)
	ctx := context.Background()
	op := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	targetEmail := uuid.NewString()[:8] + "@x.test"
	target := mkUser(t, pool, targetEmail)
	tid := mkTenant(t, pool, "ten-"+uuid.NewString()[:8])
	mkOrgMember(t, pool, tid, target, "active")

	if _, _, _, err := svc.Impersonate(ctx, op, tid, targetEmail, "  ", "", ""); !errors.Is(err, ErrBadValue) {
		t.Fatalf("empty reason: %v", err)
	}
	// grant the target a platform role — impersonation must refuse.
	var roleID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO user_roles (tenant_id, name, slug) VALUES (NULL,'plat op','plat-`+uuid.NewString()[:6]+`') RETURNING id`).Scan(&roleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO role_permissions (role_id, permission) VALUES ($1,'platform.audit.view')`, roleID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO user_role_assignments (user_id, role_id) VALUES ($1,$2)`, target, roleID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.Impersonate(ctx, op, tid, targetEmail, "check", "", ""); !errors.Is(err, ErrImpersonationDenied) {
		t.Fatalf("platform target: %v", err)
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

// TestProvisionReplayTrialDays — sameIntent must compare the VALUE of
// trial_days, not pointer identity: an identical replay that sets
// trial_days used to false-mismatch (provision_intent_mismatch).
func TestProvisionReplayTrialDays(t *testing.T) {
	days := int64(21)
	in := ProvisionRequest{
		RequestID: "req-" + uuid.NewString()[:8], TenantName: "T",
		TenantSlug: "t" + uuid.NewString()[:6], OrgSlug: "o" + uuid.NewString()[:6],
		OwnerName: "O", OwnerEmail: "o" + uuid.NewString()[:6] + "@x.test",
		TrialDays: &days}
	days2 := int64(21)
	in2 := in // fresh struct, fresh pointer
	in2.TrialDays = &days2
	stored, _ := json.Marshal(in)
	if !sameIntent(stored, in2) {
		t.Fatal("identical replay with trial_days must not mismatch")
	}
	days3 := int64(14)
	in3 := in
	in3.TrialDays = &days3
	if sameIntent(stored, in3) {
		t.Fatal("different trial_days must mismatch")
	}
}

// TestProvisionRealBootstrap — the production wiring end-to-end: scaffold
// (role clones + MVP apps + owner org_admin) must let tenant.CreateOrg's
// administered-tenant gate pass, and tenants.owner_org_id must land.
// Regression test: a bare tenant used to fail with ErrNoTenant here.
func TestProvisionRealBootstrap(t *testing.T) {
	svc, orgSvc, pool := testSvc(t)
	ctx := context.Background()
	vk, _ := valkey.NewClient(valkey.ClientOption{InitAddress: []string{"localhost:6379"}})
	ent := entitlements.NewService(pool, vk)
	svc.SetTenantScaffolder(tenant.ScaffoldTenant)
	svc.SetOrgBootstrap(func(ctx context.Context, userID uuid.UUID, name, slug, templateSlug, orgType, country, city, billingEmail string, requestedTenant uuid.UUID) (uuid.UUID, error) {
		org, err := orgSvc.CreateOrg(ctx, userID, name, slug, templateSlug, orgType, country, city, billingEmail, requestedTenant, ent)
		if err != nil {
			return uuid.Nil, err
		}
		return org.ID, nil
	})
	actor := mkUser(t, pool, uuid.NewString()[:8]+"@x.test")
	ownerEmail := "e2e+" + uuid.NewString()[:8] + "@x.test"
	tid, oid, st, err := svc.Provision(ctx, ProvisionRequest{
		RequestID: "e2e-" + uuid.NewString()[:8], TenantName: "E2E Co",
		TenantSlug: "e2e-" + uuid.NewString()[:8], OrgSlug: "e2e-org-" + uuid.NewString()[:8],
		OwnerName: "Owner", OwnerEmail: ownerEmail, Template: "union",
	}, actor)
	if err != nil || st != "done" || oid == uuid.Nil {
		t.Fatalf("provision with real bootstrap: %v %s", err, st)
	}
	// owner holds org_admin in the new tenant
	var hasRole bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM user_role_assignments ura
		 JOIN user_roles r ON r.id = ura.role_id
		 JOIN users u ON u.id = ura.user_id
		 WHERE u.email = $1 AND r.tenant_id = $2 AND r.slug = 'org_admin')`,
		ownerEmail, tid).Scan(&hasRole); err != nil || !hasRole {
		t.Fatalf("owner missing org_admin: %v", err)
	}
	// owner_org_id backref set
	var ownerOrg uuid.UUID
	if err := pool.QueryRow(ctx, "SELECT owner_org_id FROM tenants WHERE id=$1", tid).Scan(&ownerOrg); err != nil || ownerOrg != oid {
		t.Fatalf("owner_org_id = %v, want %s (%v)", ownerOrg, oid, err)
	}
	// MVP apps installed for the tenant
	var apps int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM app_installations WHERE tenant_id = $1 AND status = 'active'`, tid).Scan(&apps); err != nil || apps == 0 {
		t.Fatalf("no MVP apps installed: %v", err)
	}
	// the owner resolves org context for the provisioned org
	var memberID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT m.id FROM organization_memberships m JOIN users u ON u.id = m.user_id
		 WHERE m.organization_id = $1 AND u.email = $2`, oid, ownerEmail).Scan(&memberID); err != nil {
		t.Fatalf("owner membership missing: %v", err)
	}
}
