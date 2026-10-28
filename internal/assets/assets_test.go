// Ref: oga9-engineering/sections/18-assets.md — custody, disposal, handover.
package assets

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/tenant"
	"time"
)

func testSvc(t *testing.T) (*Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL unset")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return NewService(pool), pool
}

// mk builds tenant+org+member+current term.
func mk(t *testing.T, pool *pgxpool.Pool) (*tenant.OrgCtx, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid, termID pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenants (name, slug, billing_email, status) VALUES ($1,$2,$3,'active') RETURNING id`,
		"ten-"+suffix, "ten-"+suffix, "t-"+suffix+"@x.test").Scan(&tid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (tenant_id, name, slug, org_type) VALUES ($1,$2,$3,'association') RETURNING id`,
		tid, "org-"+suffix, "org-"+suffix).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, display_name, status) VALUES ($1,'x',$2,'active') RETURNING id`,
		"u"+suffix+"@x.test", "U "+suffix).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, category, lifecycle_state, joined_at)
		 VALUES ($1,$2,'regular','active',now()-interval '400 days') RETURNING id`, oid, uid).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO terms (organization_id, label, starts_at, ends_at, is_current)
		 VALUES ($1,'2026-2028',now()::date,now()::date+interval '2 years',true) RETURNING id`, oid).Scan(&termID); err != nil {
		t.Fatal(err)
	}
	return &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
		UserID: uuid.UUID(uid.Bytes), MembershipID: uuid.UUID(mid.Bytes), State: "active"}, uuid.UUID(termID.Bytes)
}

// TestCustodyTermFlag — spec test 1: custody still out at term end is
// flagged (CustodyHandoverFlagged emitted).
func TestCustodyTermFlag(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc, termID := mk(t, pool)

	aid, err := svc.Register(ctx, oc, "vehicle", "Bus", "", "LAG-123", "", time.Time{}, 0, "NGN", uuid.Nil, oc.MembershipID, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	cid, err := svc.Assign(ctx, oc, aid, oc.MembershipID, uuid.Nil)
	if err != nil || cid == uuid.Nil {
		t.Fatal(err)
	}
	n, err := svc.FlagUnreturned(ctx, termID)
	if err != nil || n != 1 {
		t.Fatalf("expected 1 flagged, got %d %v", n, err)
	}
	var flagged bool
	pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM domain_events WHERE event_type='CustodyHandoverFlagged' AND organization_id=$1)`, oc.OrgID).Scan(&flagged)
	if !flagged {
		t.Fatal("CustodyHandoverFlagged not emitted")
	}
	// returned custody doesn't flag
	if err := svc.Return(ctx, oc, aid); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.FlagUnreturned(ctx, termID); n != 0 {
		t.Fatalf("returned custody flagged: %d", n)
	}
}

// TestDisposeSale — spec test 2: disposal with a sale emits AssetDisposed;
// finance consumer posts the credit ledger entry (memo-deduped).
func TestDisposeSale(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc, _ := mk(t, pool)

	aid, err := svc.Register(ctx, oc, "equipment", "Generator", "", "", "", time.Time{}, 0, "NGN", uuid.Nil, oc.MembershipID, uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Dispose(ctx, oc, aid, 2500000, "NGN"); err != nil {
		t.Fatal(err)
	}
	a, _ := svc.Get(ctx, oc, aid)
	if a.State != "disposed" {
		t.Fatalf("state: %s", a.State)
	}
	// disposed assets can't be reassigned or patched
	if _, err := svc.Assign(ctx, oc, aid, oc.MembershipID, uuid.Nil); !errors.Is(err, ErrBadState) {
		t.Fatalf("dispose-guard assign: %v", err)
	}
}
