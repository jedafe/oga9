// Ref: oga9-engineering/sections/16-governance.md — motion→resolution,
// constitution gate, committee mandates.
package governance

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/tenant"
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

func mk(t *testing.T, pool *pgxpool.Pool) *tenant.OrgCtx {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO tenants (name, slug, billing_email, status) VALUES ($1,$2,$3,'active') RETURNING id`,
		"ten-"+suffix, "ten-"+suffix, "ten-"+suffix+"@x.test").Scan(&tid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organizations (tenant_id, name, slug, org_type) VALUES ($1,$2,$3,'association') RETURNING id`,
		tid, "org-"+suffix, "org-"+suffix).Scan(&oid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, password_hash, display_name, status) VALUES ($1,'x',$2,'active') RETURNING id`,
		suffix+"@x.test", "U "+suffix).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, category, lifecycle_state)
		 VALUES ($1,$2,'regular','active') RETURNING id`, oid, uid).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO terms (organization_id, label, starts_at, ends_at, is_current)
		 VALUES ($1,'2025-2027','2025-01-01','2027-01-01',true)`, oid); err != nil {
		t.Fatal(err)
	}
	return &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
		UserID: uuid.UUID(uid.Bytes), MembershipID: uuid.UUID(mid.Bytes)}
}

// TestMotionResolution — spec scenario 2: motion → vote → term-scoped
// resolution; the motion transitions out of a votable state.
func TestMotionResolution(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc := mk(t, pool)

	mid, err := svc.CreateMotion(ctx, oc, uuid.Nil, uuid.Nil, "Adopt budget", "The org adopts...")
	if err != nil {
		t.Fatal(err)
	}
	rid, err := svc.RecordVote(ctx, oc, mid, "passed", 40, 3, 2)
	if err != nil {
		t.Fatal(err)
	}
	var outcome, mstate string
	pool.QueryRow(ctx, "SELECT outcome FROM resolutions WHERE id=$1", rid).Scan(&outcome)
	pool.QueryRow(ctx, "SELECT state FROM motions WHERE id=$1", mid).Scan(&mstate)
	if outcome != "passed" || mstate != "voted" {
		t.Fatalf("outcome=%s state=%s", outcome, mstate)
	}
	// second vote on a decided motion must refuse.
	if _, err := svc.RecordVote(ctx, oc, mid, "rejected", 0, 0, 0); !errors.Is(err, ErrBadState) {
		t.Fatalf("double vote: %v", err)
	}
}

// TestConstitutionGate — spec scenario 3: adoption requires a passed
// resolution; v1 emits adopt, v2 emits amend; both stay readable.
func TestConstitutionGate(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc := mk(t, pool)

	if _, _, err := svc.AdoptConstitution(ctx, oc, "v1 text", uuid.Nil); !errors.Is(err, ErrBadValue) {
		t.Fatalf("nil resolution: %v", err)
	}
	mid, _ := svc.CreateMotion(ctx, oc, uuid.Nil, uuid.Nil, "Reject", "x")
	rid, err := svc.RecordVote(ctx, oc, mid, "rejected", 1, 40, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.AdoptConstitution(ctx, oc, "v1 text", rid); !errors.Is(err, ErrBadResolution) {
		t.Fatalf("rejected resolution adopted: %v", err)
	}
	mid2, _ := svc.CreateMotion(ctx, oc, uuid.Nil, uuid.Nil, "Adopt", "x")
	rid2, err := svc.RecordVote(ctx, oc, mid2, "passed", 40, 1, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, v1, err := svc.AdoptConstitution(ctx, oc, "v1 text", rid2)
	if err != nil || v1 != 1 {
		t.Fatalf("v1: %d %v", v1, err)
	}
	mid3, _ := svc.CreateMotion(ctx, oc, uuid.Nil, uuid.Nil, "Amend", "x")
	rid3, _ := svc.RecordVote(ctx, oc, mid3, "amended_passed", 30, 5, 0)
	_, v2, err := svc.AdoptConstitution(ctx, oc, "v2 text", rid3)
	if err != nil || v2 != 2 {
		t.Fatalf("v2: %d %v", v2, err)
	}
	cur, err := svc.CurrentConstitution(ctx, oc)
	if err != nil || cur.Version != 2 || cur.Body != "v2 text" {
		t.Fatalf("current: %d %v", cur.Version, err)
	}
	// v1 stays readable.
	list, _ := svc.ListConstitution(ctx, oc)
	if len(list) != 2 {
		t.Fatalf("versions: %d", len(list))
	}
}

// TestMandateCommitteeOnly — mandates bind to committee units only;
// non-committee units are refused.
func TestMandateCommitteeOnly(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc := mk(t, pool)

	var branchID, commID uuid.UUID
	pool.QueryRow(ctx,
		`INSERT INTO org_units (organization_id, type, name, slug) VALUES ($1,'branch','b','b1') RETURNING id`,
		oc.OrgID).Scan(&branchID)
	pool.QueryRow(ctx,
		`INSERT INTO org_units (organization_id, type, name, slug) VALUES ($1,'committee','c','c1') RETURNING id`,
		oc.OrgID).Scan(&commID)

	if err := svc.UpsertMandate(ctx, oc, branchID, "mandate", uuid.Nil, 0, "", true); !errors.Is(err, ErrNotCommittee) {
		t.Fatalf("branch mandate: %v", err)
	}
	if err := svc.UpsertMandate(ctx, oc, commID, "welfare committee", uuid.Nil, 500000, "NGN", true); err != nil {
		t.Fatalf("committee mandate: %v", err)
	}
	limit, cur, ok := svc.MandateLimit(ctx, commID)
	if !ok || limit != 500000 || cur != "NGN" {
		t.Fatalf("limit: %d %s %v", limit, cur, ok)
	}
}
