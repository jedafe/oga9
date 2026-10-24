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

	org, err := svc.CreateOrg(ctx, owner, "Test Union", "tu"+uuid.NewString()[:6], "union", "union", "NG", "Lagos", "owner@test.oga9")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	oc, err := svc.ResolveOrgCtx(ctx, owner, org.ID)
	if err != nil {
		t.Fatalf("resolve ctx: %v", err)
	}
	if !svc.HasPermission(ctx, owner, oc.TenantID, "org.structure.manage") {
		t.Fatal("creator missing org.structure.manage")
	}
	if _, err := svc.ResolveOrgCtx(ctx, stranger, org.ID); err != ErrNotMember {
		t.Fatalf("stranger should get not_a_member, got %v", err)
	}

	// units: parent, child, cycle rejected, archive guard
	hq, err := svc.CreateUnit(ctx, oc, uuid.Nil, "hq", "general", "HQ", "hq")
	if err != nil {
		t.Fatalf("unit: %v", err)
	}
	br, err := svc.CreateUnit(ctx, oc, hq, "branch", "lagos", "Lagos Branch", "lagos")
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
	if _, err := svc.AcceptInvitation(ctx, stranger, inv.Token); err != ErrForbidden {
		t.Fatalf("wrong-email accept should be forbidden, got %v", err)
	}
	if _, err := svc.AcceptInvitation(ctx, member, inv.Token); err != nil {
		t.Fatalf("accept: %v", err)
	}
	moc, err := svc.ResolveOrgCtx(ctx, member, org.ID)
	if err != nil {
		t.Fatalf("member ctx: %v", err)
	}
	if !svc.HasPermission(ctx, member, moc.TenantID, "org.members.view") {
		t.Fatal("accepted member missing org.members.view")
	}
	if svc.HasPermission(ctx, member, moc.TenantID, "org.structure.manage") {
		t.Fatal("member should not have admin node")
	}

	// events recorded
	var n int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM domain_events WHERE name IN ('OrganizationCreated','TemplateInstantiated','MemberInvited','InvitationAccepted') AND occurred_at > $1",
		time.Now().Add(-time.Minute)).Scan(&n); err != nil || n < 4 {
		t.Fatalf("expected org events in outbox, got %d (err %v)", n, err)
	}
}
