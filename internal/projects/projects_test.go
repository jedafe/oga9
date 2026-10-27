// Ref: oga9-engineering/sections/20-projects.md — resolution gate,
// weighted milestones, completion report.
package projects

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
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

func mk(t *testing.T, pool *pgxpool.Pool) (*tenant.OrgCtx, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid, termID, resID pgtype.UUID
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
	// a passed resolution exists for the approval path
	if err := pool.QueryRow(ctx,
		`INSERT INTO resolutions (organization_id, term_id, title, body, outcome)
		 VALUES ($1,$2,'Build hall','Approve the project','passed') RETURNING id`,
		oid, termID).Scan(&resID); err != nil {
		t.Fatal(err)
	}
	return &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
		UserID: uuid.UUID(uid.Bytes), MembershipID: uuid.UUID(mid.Bytes), State: "active"}, uuid.UUID(resID.Bytes)
}

// TestResolutionGate — spec test 1: starting without a passed resolution
// is blocked.
func TestResolutionGate(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc, resID := mk(t, svc.pool)

	pid, err := svc.Propose(ctx, oc, "Community hall", "build", uuid.Nil, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	// proposed → start blocked (no approval edge yet)
	if err := svc.Transition(ctx, oc, pid, "start"); !errors.Is(err, ErrNoResolution) {
		t.Fatalf("start without approval: %v", err)
	}
	// approval with a bogus resolution id also fails
	if err := svc.Approve(ctx, oc, pid, uuid.New()); !errors.Is(err, ErrNoResolution) {
		t.Fatalf("approve with fake resolution: %v", err)
	}
	// passed resolution → approved → start
	if err := svc.Approve(ctx, oc, pid, resID); err != nil {
		t.Fatal(err)
	}
	if err := svc.Transition(ctx, oc, pid, "start"); err != nil {
		t.Fatal(err)
	}
	p, _ := svc.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if p.State != "active" {
		t.Fatalf("state: %s", p.State)
	}
}

// TestMilestoneProgress — spec test 2: weighted milestones recompute
// project progress.
func TestMilestoneProgress(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc, resID := mk(t, svc.pool)

	pid, _ := svc.Propose(ctx, oc, "Borehole", "", uuid.Nil, time.Time{}, time.Time{})
	_ = svc.Approve(ctx, oc, pid, resID)
	_ = svc.Transition(ctx, oc, pid, "start")

	m1, _ := svc.AddMilestone(ctx, oc, pid, "Survey", 1, time.Time{})
	m2, _ := svc.AddMilestone(ctx, oc, pid, "Drill", 3, time.Time{})

	if err := svc.CompleteMilestone(ctx, oc, pid, m1); err != nil {
		t.Fatal(err)
	}
	p, _ := svc.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if p.ProgressPct != 25 {
		t.Fatalf("expected 25%%, got %d", p.ProgressPct)
	}
	if err := svc.CompleteMilestone(ctx, oc, pid, m2); err != nil {
		t.Fatal(err)
	}
	p, _ = svc.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if p.ProgressPct != 100 {
		t.Fatalf("expected 100%%, got %d", p.ProgressPct)
	}
}

// TestCloseReport — spec test 4: close completes + archives the report
// (document seam) + entity_links edge exists.
func TestCloseReport(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc, resID := mk(t, svc.pool)

	pid, _ := svc.Propose(ctx, oc, "Bridge", "", uuid.Nil, time.Time{}, time.Time{})
	_ = svc.Approve(ctx, oc, pid, resID)
	_ = svc.Transition(ctx, oc, pid, "start")
	if err := svc.Close(ctx, oc, pid); err != nil {
		t.Fatal(err)
	}
	p, _ := svc.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if p.State != "completed" {
		t.Fatalf("state: %s", p.State)
	}
	graph, _ := svc.Graph(ctx, oc, pid)
	if len(graph) == 0 {
		t.Fatal("expected entity_links edges (approval/report)")
	}
}
