// Ref: oga9-engineering/adr/ADR-020 + Rule 15/24 — cross-tenant isolation
// suite. Two orgs on separate tenants; every scoped surface must refuse
// foreign-org access — at ctx resolution AND at the SQL layer (a forged
// OrgCtx pairing org B with org A's membership must yield not-found/empty).
package isolation

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/activities"
	"github.com/jedafe/oga9/internal/chat"
	"github.com/jedafe/oga9/internal/comms"
	"github.com/jedafe/oga9/internal/finance"
	"github.com/jedafe/oga9/internal/storage"
	"github.com/jedafe/oga9/internal/tenant"
)

type env struct {
	org  *tenant.Service
	chat *chat.Service
	fin  *finance.Service
	act  *activities.Service
	docs *comms.Service
	pool *pgxpool.Pool
}

func testEnv(t *testing.T) *env {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil || pool.Ping(context.Background()) != nil {
		t.Skip("test db unavailable")
	}
	vk, _ := valkey.NewClient(valkey.ClientOption{InitAddress: []string{"localhost:6379"}})
	docs := comms.NewService(pool)
	docs.SetStore(storage.New(t.TempDir()))
	return &env{
		org:  tenant.NewService(pool, vk),
		chat: chat.NewService(pool, vk),
		fin:  finance.NewService(pool, vk),
		act:  activities.NewService(pool),
		docs: docs,
		pool: pool,
	}
}

func mkUser(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		"INSERT INTO users (id,email,password_hash,display_name,status) VALUES ($1,$2,'x','T','active')",
		id, "iso"+uuid.NewString()[:8]+"@t.co"); err != nil {
		t.Fatal(err)
	}
	return id
}

// mkOrg creates an org (own tenant) with an active owner membership.
func mkOrg(t *testing.T, e *env) (uuid.UUID, *tenant.OrgCtx) {
	t.Helper()
	ctx := context.Background()
	owner := mkUser(t, e.pool)
	org, err := e.org.CreateOrg(ctx, owner, "Iso Org", "iso"+uuid.NewString()[:6],
		"union", "union", "NG", "Jos", "iso@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.pool.Exec(ctx,
		"UPDATE organization_memberships SET lifecycle_state='active' WHERE organization_id=$1", org.ID); err != nil {
		t.Fatal(err)
	}
	oc, err := e.org.ResolveOrgCtx(ctx, owner, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	return org.ID, oc
}

// forge builds a request-level context the way a bypassed/forged layer would:
// org B's id paired with org A's membership — every scoped query must
// still resolve to org B (and find nothing of A's).
func forge(ocA *tenant.OrgCtx, orgB, tenantB uuid.UUID) *tenant.OrgCtx {
	return &tenant.OrgCtx{
		OrgID:        orgB,
		TenantID:     tenantB,
		UserID:       ocA.UserID,
		MembershipID: ocA.MembershipID,
	}
}

func TestOrgCtxResolution(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, _ := mkOrg(t, e)
	_ = orgA

	// member of A is not a member of B — ctx resolution refuses.
	if _, err := e.org.ResolveOrgCtx(ctx, ocA.UserID, orgB); !errors.Is(err, tenant.ErrNotMember) {
		t.Fatalf("expected ErrNotMember, got %v", err)
	}
}

func TestChatChannelIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)

	chB, err := e.chat.CreateChannel(ctx, ocB, "general", "", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	// WS subscribe with a foreign membership — the review's real bug.
	if e.chat.CanSubscribe(ctx, chB, ocA.MembershipID, ocA.UserID) {
		t.Fatal("org A membership subscribed to org B channel — cross-tenant leak")
	}
	// Channel lists are org-scoped.
	list, err := e.chat.Channels(ctx, ocA)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.OrganizationID.Bytes != orgA {
			t.Fatalf("channel list leaked a foreign channel into org %s", orgA)
		}
	}
	// Posting to a foreign channel through a forged ctx must fail.
	forged := forge(ocA, orgB, ocB.TenantID)
	if _, err := e.chat.PostMessage(ctx, forged, chB, "text", "hi", uuid.Nil, uuid.Nil, 0); !errors.Is(err, chat.ErrNotMember) {
		t.Fatalf("expected ErrNotMember posting cross-org, got %v", err)
	}
}

func TestFinanceIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	forged := forge(ocA, orgB, ocB.TenantID)

	// Statement under a forged ctx resolves only org B's ledger (empty).
	st, err := e.fin.Statement(ctx, forged)
	if err == nil && st != nil && len(st.ByAccount) != 0 {
		t.Fatalf("forged ctx statement returned %d ledger accounts", len(st.ByAccount))
	}
	// RecordPayment: foreign membership must not write into org B.
	_, _, err = e.fin.RecordPayment(ctx, forged, finance.PaymentIn{
		AmountMinor: 1000, Currency: "NGN", Channel: "cash"})
	if err == nil {
		t.Fatal("RecordPayment accepted a foreign membership under a forged ctx")
	}
}

func TestActivitiesIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)

	aid, _, err := e.act.CreateActivity(ctx, ocA, activities.ActivityIn{
		Kind: "meeting", Title: "A's meeting", StartsAt: time.Now().Add(48 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	if _, err := e.act.Get(ctx, forged, aid); !errors.Is(err, activities.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-org activity read, got %v", err)
	}
}

func TestMembersIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	forged := forge(ocA, orgB, ocB.TenantID)

	// org A's membership must not appear in org B's list under a forged
	// ctx (org B legitimately has its own owner member).
	var memberA uuid.UUID
	if err := e.pool.QueryRow(ctx,
		"SELECT id FROM organization_memberships WHERE organization_id=$1 LIMIT 1", orgA).Scan(&memberA); err != nil {
		t.Fatal(err)
	}
	list, err := e.org.Members(ctx, forged, "", "", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range list {
		if uuid.UUID(m.ID.Bytes) == memberA {
			t.Fatal("forged ctx listed an org A member — cross-tenant leak")
		}
	}
	// member detail of org A's membership under org B ctx → not found.
	if _, err := e.org.MemberDetail(ctx, forged, memberA); err == nil {
		t.Fatal("forged ctx read a foreign membership")
	}
}

func TestAnnouncementsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	forged := forge(ocA, orgB, ocB.TenantID)

	if _, err := e.pool.Exec(ctx,
		`INSERT INTO announcements (organization_id, author_id, title, body, state)
		 VALUES ($1,$2,'secret','org A only','published')`, orgA, ocA.MembershipID); err != nil {
		t.Fatal(err)
	}
	list, err := e.docs.List(ctx, forged, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A announcements", len(list))
	}
}

func TestDocumentsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)

	did, err := e.docs.Upload(ctx, ocA, "report", "A report", "members", uuid.Nil,
		strings.NewReader("org A only"))
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	if _, _, err := e.docs.Download(ctx, forged, did, false); !errors.Is(err, comms.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for cross-org download, got %v", err)
	}
	list, err := e.docs.Documents(ctx, forged, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A documents", len(list))
	}
	// A foreign unit_id on upload is rejected, not stored.
	unit := uuid.New() // nonexistent — and certainly not in org A
	if _, err := e.docs.Upload(ctx, ocA, "report", "x", "members", unit,
		strings.NewReader("y")); !errors.Is(err, comms.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for foreign unit_id, got %v", err)
	}
}
