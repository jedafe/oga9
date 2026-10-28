package tenant

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/entitlements"
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

func mkUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	q := db.New(pool)
	r, err := q.CreateUser(context.Background(), db.CreateUserParams{
		Email: email, PasswordHash: pgtype.Text{Valid: true, String: "x"}, DisplayName: "T",
	})
	if err != nil {
		t.Fatal(err)
	}
	return uuid.UUID(r.ID.Bytes)
}

func TestOrgLifecycleFlow(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "owner"+uuid.NewString()[:8]+"@test.oga9")
	memberEmail := "mem" + uuid.NewString()[:8] + "@test.oga9"
	member := mkUser(t, pool, memberEmail)
	stranger := mkUser(t, pool, "stg"+uuid.NewString()[:8]+"@test.oga9")

	ent := entitlements.NewService(pool, svc.vk)
	org, err := svc.CreateOrg(ctx, owner, "Test Union", "tu"+uuid.NewString()[:6], "union", "union", "NG", "Lagos", "owner@test.oga9", uuid.Nil, ent)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	oc, err := svc.ResolveOrgCtx(ctx, owner, org.ID)
	if err != nil {
		t.Fatalf("resolve ctx: %v", err)
	}
	// §07 limit gate: starter allows limit.organizations=1 — a second org on
	// the same tenant is denied until the plan offers more headroom.
	if _, err := svc.CreateOrg(ctx, owner, "Starter Blocked", "sb"+uuid.NewString()[:6], "union", "union", "NG", "Enugu", "", uuid.Nil, ent); err != ErrLimitExceeded {
		t.Fatalf("starter second org should hit limit.organizations, got %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE subscriptions SET plan_slug='professional' WHERE tenant_id=$1", org.TenantID); err != nil {
		t.Fatal(err)
	}
	ent.Invalidate(ctx, org.TenantID) // plan change → drop the cached starter set
	// second org by the same owner attaches to their administered tenant
	org2same, err := svc.CreateOrg(ctx, owner, "Owner Second", "os"+uuid.NewString()[:6], "union", "union", "NG", "Enugu", "", uuid.Nil, ent)
	if err != nil {
		t.Fatalf("create second org: %v", err)
	}
	if org2same.TenantID != org.TenantID {
		t.Fatal("second org should attach to the caller's existing tenant")
	}
	// a foreign tenant_id is verified server-side — member can't use it
	if _, err := svc.CreateOrg(ctx, member, "Hijack", "hj"+uuid.NewString()[:6], "union", "union", "NG", "X", "x@t.co", org.TenantID, ent); err != ErrNoTenant {
		t.Fatalf("foreign tenant_id should be rejected, got %v", err)
	}
	if !svc.HasPermission(ctx, owner, org.ID, "org.structure.manage") {
		t.Fatal("creator missing org.structure.manage")
	}
	if _, err := svc.ResolveOrgCtx(ctx, stranger, org.ID); err != ErrNotMember {
		t.Fatalf("stranger should get not_a_member, got %v", err)
	}

	// units: parent, child, cycle rejected, archive guard
	hq, err := svc.CreateUnit(ctx, oc, uuid.Nil, "hq", "general", "HQ", "hq", ent)
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	br, err := svc.CreateUnit(ctx, oc, hq, "branch", "lagos", "Lagos Branch", "lagos", ent)
	if err != nil {
		t.Fatalf("branch: %v", err)
	}
	if err := svc.UpdateUnit(ctx, oc, hq, br, "hq", "", "HQ"); err != ErrUnitCycle {
		t.Fatalf("cycle should be rejected, got %v", err)
	}
	if err := svc.ArchiveUnit(ctx, oc, hq); err != ErrUnitDependents {
		t.Fatalf("archive with child should fail, got %v", err)
	}
	if err := svc.ArchiveUnit(ctx, oc, br); err != nil {
		t.Fatalf("archive leaf: %v", err)
	}

	// settings catalog gate
	if err := svc.SetSetting(ctx, oc, "org.meeting.quorum_percent", []byte("33")); err != nil {
		t.Fatalf("catalog setting: %v", err)
	}
	if err := svc.SetSetting(ctx, oc, "bogus.key", []byte("1")); err != ErrUnknownSetting {
		t.Fatalf("unknown key should fail, got %v", err)
	}

	// invitation: wrong email rejected, right member gets member role
	inv, err := svc.CreateInvitation(ctx, oc, memberEmail, "", "", "floor")
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, stranger, inv.Token, ""); err != ErrForbidden {
		t.Fatalf("wrong-email accept should be forbidden, got %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, member, inv.Token, ""); err != nil {
		t.Fatalf("accept: %v", err)
	}
	moc, err := svc.ResolveOrgCtx(ctx, member, org.ID)
	if err != nil {
		t.Fatalf("member ctx: %v", err)
	}
	if moc.OrgID != org.ID {
		t.Fatal("member ctx org mismatch")
	}
	if !svc.HasPermission(ctx, member, org.ID, "org.members.view") {
		t.Fatal("accepted member missing org.members.view")
	}
	if svc.HasPermission(ctx, member, org.ID, "org.structure.manage") {
		t.Fatal("member should not have admin node")
	}

	// cross-org invite revoke must not reach another org's invitation
	inv2, err := svc.CreateInvitation(ctx, oc, "gone-"+uuid.NewString()[:8]+"@test.oga9", "", "", "floor")
	if err != nil {
		t.Fatalf("invite2: %v", err)
	}
	org2, err := svc.CreateOrg(ctx, stranger, "Other Club", "oc"+uuid.NewString()[:6], "social-club", "", "NG", "Abuja", "stg@test.oga9", uuid.Nil, ent)
	if err != nil {
		t.Fatalf("create org2: %v", err)
	}
	oc2, err := svc.ResolveOrgCtx(ctx, stranger, org2.ID)
	if err != nil {
		t.Fatalf("resolve ctx2: %v", err)
	}
	if err := svc.RevokeInvitation(ctx, oc2, inv2.ID); err != ErrInviteNotFound {
		t.Fatalf("cross-org revoke should be invitation_not_found, got %v", err)
	}
	if err := svc.RevokeInvitation(ctx, oc, inv2.ID); err != nil {
		t.Fatalf("own-org revoke: %v", err)
	}
	if err := svc.RevokeInvitation(ctx, oc, inv2.ID); err != ErrInviteNotFound {
		t.Fatalf("revoking a revoked invite should be 404, got %v", err)
	}

	// events recorded
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM domain_events WHERE event_type IN ('OrganizationCreated','TemplateInstantiated','MemberInvited','InvitationAccepted') AND occurred_at > $1",
		time.Now().Add(-time.Minute)).Scan(&n); err != nil || n < 4 {
		t.Fatalf("expected org events in outbox, got %d (err %v)", n, err)
	}
}
