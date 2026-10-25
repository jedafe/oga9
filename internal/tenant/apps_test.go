package tenant

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/entitlements"
)

func countEvent(t *testing.T, svc *Service, tenantID uuid.UUID, eventType, appID string) int {
	t.Helper()
	var n int
	if err := svc.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM domain_events WHERE event_type=$1 AND tenant_id=$2 AND payload->>'app_id'=$3`,
		eventType, tenantID, appID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAppLifecycle(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "appown"+uuid.NewString()[:8]+"@test.oga9")
	org, err := svc.CreateOrg(ctx, owner, "Apps Union", "ap"+uuid.NewString()[:6], "union", "union", "NG", "Lagos", "o@t.co", uuid.Nil)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	oc, err := svc.ResolveOrgCtx(ctx, owner, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	ent := entitlements.NewService(pool, svc.vk)

	// provision installed all mvp_required apps
	apps, err := svc.OrgApps(ctx, oc)
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, a := range apps {
		status[a.AppID] = a.Status
	}
	for _, id := range []string{"organization_core", "members", "comms_documents", "subscription_admin"} {
		if status[id] != "active" {
			t.Fatalf("%s should be provisioned active, got %s", id, status[id])
		}
	}
	if status["finance_lite"] != "not_installed" || status["events_meetings"] != "not_installed" {
		t.Fatalf("non-required apps should not auto-install: %v", status)
	}

	// mvp_required apps can't deactivate at all (every requires-target in the
	// MVP seed graph is required, so app_required preempts dependents there)
	for _, id := range []string{"organization_core", "members", "comms_documents", "subscription_admin"} {
		if err := svc.DeactivateApp(ctx, oc, id); err != ErrAppRequired {
			t.Fatalf("%s is mvp_required, got %v", id, err)
		}
	}

	// synthetic non-required pair exercises the dependents path
	base := "pb" + uuid.NewString()[:8]
	ext := "pe" + uuid.NewString()[:8]
	baseM, _ := json.Marshal(map[string]any{"id": base, "version": "1.0.0", "requires": []string{}, "entitlements": map[string]any{}})
	extM, _ := json.Marshal(map[string]any{"id": ext, "version": "1.0.0", "requires": []string{base}, "entitlements": map[string]any{}})
	if _, err := pool.Exec(ctx, `INSERT INTO app_registry (app_id, version, label, manifest) VALUES
		($1,'1.0.0','Base',$3::jsonb), ($2,'1.0.0','Ext',$4::jsonb)`, base, ext, baseM, extM); err != nil {
		t.Fatal(err)
	}
	if err := svc.ActivateApp(ctx, oc, ent, base); err != nil {
		t.Fatalf("no requires, no feature gate → activate, got %v", err)
	}
	if err := svc.ActivateApp(ctx, oc, ent, ext); err != nil {
		t.Fatalf("dep active → activate, got %v", err)
	}
	if err := svc.DeactivateApp(ctx, oc, base); err != ErrAppHasDependents {
		t.Fatalf("ext depends on base, got %v", err)
	}
	if err := svc.DeactivateApp(ctx, oc, ext); err != nil {
		t.Fatalf("deactivate leaf first: %v", err)
	}
	if err := svc.DeactivateApp(ctx, oc, base); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if ok, _ := svc.IsAppActive(ctx, oc.TenantID, base); ok {
		t.Fatal("base should be inactive")
	}
	if n := countEvent(t, svc, oc.TenantID, "AppDeactivated", base); n != 1 {
		t.Fatalf("expected 1 AppDeactivated, got %d", n)
	}

	// repeat deactivation is a no-op — no phantom event
	if err := svc.DeactivateApp(ctx, oc, base); err != nil {
		t.Fatal(err)
	}
	if n := countEvent(t, svc, oc.TenantID, "AppDeactivated", base); n != 1 {
		t.Fatalf("no-op deactivate emitted phantom events: %d", n)
	}
	// never-installed app: also a silent no-op
	if err := svc.DeactivateApp(ctx, oc, "finance_lite"); err != nil {
		t.Fatal(err)
	}
	if n := countEvent(t, svc, oc.TenantID, "AppDeactivated", "finance_lite"); n != 0 {
		t.Fatalf("never-installed deactivate emitted %d phantom events", n)
	}

	// entitlement gate: starter plan doesn't bundle finance_lite
	if err := svc.ActivateApp(ctx, oc, ent, "finance_lite"); err != ErrNotEntitled {
		t.Fatalf("starter lacks app.finance_lite, got %v", err)
	}
	// dependency gate: ext requires base (currently inactive)
	if err := svc.ActivateApp(ctx, oc, ent, ext); err != ErrAppDepsMissing {
		t.Fatalf("ext needs base active, got %v", err)
	}
	if err := svc.ActivateApp(ctx, oc, ent, base); err != nil {
		t.Fatalf("reactivate base: %v", err)
	}
	if err := svc.ActivateApp(ctx, oc, ent, ext); err != nil {
		t.Fatalf("deps satisfied → activate, got %v", err)
	}
	if err := svc.ActivateApp(ctx, oc, ent, "no_such_app"); err != ErrAppNotFound {
		t.Fatalf("unknown app, got %v", err)
	}
	// re-activating an already-active app is a no-op — no phantom event
	if err := svc.ActivateApp(ctx, oc, ent, "members"); err != nil {
		t.Fatal(err)
	}
	if n := countEvent(t, svc, oc.TenantID, "AppActivated", "members"); n != 0 {
		t.Fatalf("re-activation emitted %d phantom events", n)
	}
}

// platform_admin is control-plane only — the tenant role clone must never
// carry platform.* nodes into a provisioned tenant.
func TestNoPlatformAdminClone(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "padm"+uuid.NewString()[:8]+"@test.oga9")
	org, err := svc.CreateOrg(ctx, owner, "Clone Union", "cl"+uuid.NewString()[:6], "union", "union", "NG", "Lagos", "o@t.co", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM user_roles r JOIN role_permissions rp ON rp.role_id = r.id
		 WHERE r.tenant_id = $1 AND rp.permission LIKE 'platform.%'`, org.TenantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("tenant holds %d platform.* grants via cloned roles", n)
	}
}

// Billing-tenant resolution is node-based (org.admin.billing), not a role
// name — api-routes-catalog + Rule 20.
func TestTenantsWithNode(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "nodew"+uuid.NewString()[:8]+"@test.oga9")
	stranger := mkUser(t, pool, "nodex"+uuid.NewString()[:8]+"@test.oga9")
	org, err := svc.CreateOrg(ctx, owner, "Node Union", "nd"+uuid.NewString()[:6], "union", "union", "NG", "Lagos", "o@t.co", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := svc.TenantsWithNode(ctx, owner, "org.admin.billing")
	if err != nil || len(ids) != 1 || ids[0] != org.TenantID {
		t.Fatalf("owner should resolve billing tenant, got %v %v", ids, err)
	}
	if ids, _ := svc.TenantsWithNode(ctx, stranger, "org.admin.billing"); len(ids) != 0 {
		t.Fatal("stranger holds no billing node")
	}
}
