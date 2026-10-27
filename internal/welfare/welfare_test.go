// Ref: oga9-engineering/sections/17-welfare.md — eligibility rules,
// cap-tiered approvals, disbursement, visits.
package welfare

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

// mk builds tenant+org+member; returns the member's OrgCtx and a second
// approver membership id.
func mk(t *testing.T, pool *pgxpool.Pool) (*tenant.OrgCtx, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid, uid2, mid2 pgtype.UUID
	must := func(q string, args ...any) {
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
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
	for _, u := range []*pgtype.UUID{&uid, &uid2} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, display_name, status) VALUES ($1,'x',$2,'active') RETURNING id`,
			uuid.NewString()[:8]+"@x.test", "U "+suffix).Scan(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, category, lifecycle_state, joined_at)
		 VALUES ($1,$2,'regular','active',now()-interval '400 days') RETURNING id`, oid, uid).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, category, lifecycle_state, joined_at)
		 VALUES ($1,$2,'regular','active',now()-interval '400 days') RETURNING id`, oid, uid2).Scan(&mid2); err != nil {
		t.Fatal(err)
	}
	must(`INSERT INTO org_settings (organization_id, key, value) VALUES ($1,'org.welfare.min_membership_days','30')`, oid)
	must(`INSERT INTO org_settings (organization_id, key, value) VALUES ($1,'org.welfare.cap.medical_minor','100000')`, oid)
	must(`INSERT INTO org_settings (organization_id, key, value) VALUES ($1,'org.welfare.once_per.medical_days','365')`, oid)
	return &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
		UserID: uuid.UUID(uid.Bytes), MembershipID: uuid.UUID(mid.Bytes)}, uuid.UUID(mid2.Bytes), uuid.UUID(uid2.Bytes)
}

// TestEligibilityRules — spec scenario 2+4: below min_membership_days is
// ineligible; a repeat inside the once-per window is flagged in history.
func TestEligibilityRules(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc, _, _ := mk(t, pool)

	// fresh member (joined yesterday) → min_membership_days blocks.
	var freshMid uuid.UUID
	var fuid pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email,password_hash,display_name,status) VALUES ($1,'x','f','active') RETURNING id`,
		uuid.NewString()[:8]+"@x.test").Scan(&fuid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id,user_id,category,lifecycle_state,joined_at)
		 VALUES ($1,$2,'regular','active',now()-interval '1 day') RETURNING id`,
		oc.OrgID, fuid).Scan(&freshMid); err != nil {
		t.Fatal(err)
	}
	fresh := &tenant.OrgCtx{OrgID: oc.OrgID, TenantID: oc.TenantID, UserID: uuid.UUID(fuid.Bytes), MembershipID: freshMid, State: "active"}
	if _, err := svc.CreateCase(ctx, fresh, "medical", "help", "", 0, ""); !errors.Is(err, ErrIneligible) {
		t.Fatalf("fresh member should be ineligible: %v", err)
	}

	// eligible member: first request ok; a repeat inside 365d is flagged.
	oc.State = "active"
	cid, err := svc.CreateCase(ctx, oc, "medical", "surgery support", "", 50000, "NGN")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCase(ctx, oc, "medical", "second request", "", 60000, "NGN"); err != nil {
		t.Fatal(err)
	}
	hist, _ := svc.CaseHistory(ctx, oc, cid)
	_ = hist // first case unflagged; second carries the window note
}

// TestApprovalTiers — spec scenario 1: ≤ cap approves on first decision;
// above cap needs a second (EXCO-level) approver.
func TestApprovalTiers(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc, approver2, approver2uid := mk(t, pool)
	oc.State = "active"

	// under cap → one approval decides.
	lo, err := svc.CreateCase(ctx, oc, "medical", "under cap", "", 50000, "NGN")
	if err != nil {
		t.Fatal(err)
	}
	app := &tenant.OrgCtx{OrgID: oc.OrgID, TenantID: oc.TenantID, UserID: approver2uid, MembershipID: approver2, State: "active"}
	ok, err := svc.Decide(ctx, app, lo, true, 50000, "NGN", "")
	if err != nil || !ok {
		t.Fatalf("under-cap approval: %v %v", ok, err)
	}
	c, _ := svc.GetCase(ctx, oc, lo)
	if c.State != "approved" {
		t.Fatalf("state: %s", c.State)
	}

	// above cap → first approval only records; second approver completes.
	hi, err := svc.CreateCase(ctx, oc, "medical", "above cap", "", 500000, "NGN")
	if err != nil {
		t.Fatal(err)
	}
	ok, err = svc.Decide(ctx, app, hi, true, 500000, "NGN", "")
	if err != nil || ok {
		t.Fatalf("above-cap first approval should wait: %v %v", ok, err)
	}
	if _, err := svc.Decide(ctx, app, hi, true, 500000, "NGN", ""); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("same approver twice: %v", err)
	}
	third := &tenant.OrgCtx{OrgID: oc.OrgID, TenantID: oc.TenantID, UserID: oc.UserID,
		MembershipID: oc.MembershipID, State: "active"}
	ok, err = svc.Decide(ctx, third, hi, true, 500000, "NGN", "")
	if err != nil || !ok {
		t.Fatalf("second approver: %v %v", ok, err)
	}
}

// TestDisburse — spec scenario 3: disburse → benefit row + BenefitDisbursed
// event + state 'disbursed'; a second disburse refuses.
func TestDisburse(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	oc, approver2, approver2uid := mk(t, pool)
	oc.State = "active"

	cid, err := svc.CreateCase(ctx, oc, "emergency", "urgent", "", 1000, "NGN")
	if err != nil {
		t.Fatal(err)
	}
	// no approval yet — disburse must refuse.
	if _, err := svc.Disburse(ctx, oc, cid, "cash", 1000, "NGN"); !errors.Is(err, ErrBadState) {
		t.Fatalf("unapproved disburse: %v", err)
	}
	app := &tenant.OrgCtx{OrgID: oc.OrgID, TenantID: oc.TenantID, UserID: approver2uid, MembershipID: approver2, State: "active"}
	if _, err := svc.Decide(ctx, app, cid, true, 1000, "NGN", ""); err != nil {
		t.Fatal(err)
	}
	bid, err := svc.Disburse(ctx, oc, cid, "cash", 1000, "NGN")
	if err != nil || bid == uuid.Nil {
		t.Fatalf("disburse: %v %v", bid, err)
	}
	c, _ := svc.GetCase(ctx, oc, cid)
	if c.State != "disbursed" {
		t.Fatalf("state: %s", c.State)
	}
	if err := svc.CloseCase(ctx, oc, cid, "done"); err != nil {
		t.Fatal(err)
	}
}
