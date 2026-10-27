// Ref: oga9-engineering/sections/19-analytics.md — snapshot serve, health
// compute, consolidation gate.
package analytics

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

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

func mkOrg(t *testing.T, pool *pgxpool.Pool) *tenant.OrgCtx {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid pgtype.UUID
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
	return &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes)}
}

// TestDashboardServe — snapshot build-on-miss + refresh produces a stored
// snapshot per role.
func TestDashboardServe(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkOrg(t, svc.pool)

	raw, err := svc.Dashboard(ctx, oc, "member")
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	if json.Unmarshal(raw, &snap) != nil || snap["role"] != "member" {
		t.Fatalf("bad snapshot: %v", snap)
	}
	// refresh writes all four role snapshots.
	if _, err := svc.RefreshSnapshots(ctx); err != nil {
		t.Fatal(err)
	}
	for _, role := range snapshotRoles {
		if _, err := svc.q.GetSnapshot(ctx, db.GetSnapshotParams{
			OrganizationID: toPg(oc.OrgID), Role: role}); err != nil {
			t.Fatalf("missing %s snapshot: %v", role, err)
		}
	}
}

// TestHealthCompute — components land with the versioned formula; KPI
// thresholds breach → KPIThresholdCrossed.
func TestHealthCompute(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkOrg(t, svc.pool)

	if _, err := svc.pool.Exec(ctx, `INSERT INTO kpi_definitions (organization_id, key, formula) VALUES ($1,'attendance_floor','{"component":"attendance","op":"lt","value":10}')`, oc.OrgID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ComputeHealthScores(ctx); err != nil {
		t.Fatal(err)
	}
	h, err := svc.Health(ctx, oc)
	if err != nil {
		t.Fatal(err)
	}
	if h.FormulaVersion != "v1" || h.Score < 0 || h.Score > 100 {
		t.Fatalf("health: %+v", h)
	}
	var crossed bool
	svc.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM domain_events WHERE event_type='KPIThresholdCrossed' AND organization_id=$1)`, oc.OrgID).Scan(&crossed)
	if !crossed {
		t.Fatal("expected KPIThresholdCrossed (attendance=0 < 10)")
	}
}

// TestConsolidatedGate — spec test 4: non-Business → capability_required.
func TestConsolidatedGate(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkOrg(t, svc.pool)

	svc.SetCapability(func(context.Context, uuid.UUID, string) bool { return false })
	if _, err := svc.Consolidated(ctx, oc); !errors.Is(err, ErrCapability) {
		t.Fatalf("expected capability_required, got %v", err)
	}
	svc.SetCapability(func(context.Context, uuid.UUID, string) bool { return true })
	if _, err := svc.Consolidated(ctx, oc); err != nil {
		t.Fatalf("consolidated: %v", err)
	}
}
