// Ref: oga9-engineering/sections/22-ai.md — quota, draft lifecycle, metering.
package ai

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

func mkCtx(t *testing.T, pool *pgxpool.Pool) *tenant.OrgCtx {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid pgtype.UUID
	pool.QueryRow(ctx, `INSERT INTO tenants (name,slug,billing_email,status) VALUES ($1,$2,$3,'active') RETURNING id`,
		"t-"+suffix, "t-"+suffix, "t-"+suffix+"@x.test").Scan(&tid)
	pool.QueryRow(ctx, `INSERT INTO organizations (tenant_id,name,slug,org_type) VALUES ($1,$2,$3,'association') RETURNING id`,
		tid, "o-"+suffix, "o-"+suffix).Scan(&oid)
	pool.QueryRow(ctx, `INSERT INTO users (email,password_hash,display_name,status) VALUES ($1,'x','u','active') RETURNING id`,
		"u-"+suffix+"@x.test").Scan(&uid)
	pool.QueryRow(ctx, `INSERT INTO organization_memberships (organization_id,user_id,category,lifecycle_state) VALUES ($1,$2,'regular','active') RETURNING id`,
		oid, uid).Scan(&mid)
	return &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
		UserID: uuid.UUID(uid.Bytes), MembershipID: uuid.UUID(mid.Bytes)}
}

// TestQuota — spec test 3: limit hit → hard stop + AIQuotaExceeded.
func TestQuota(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkCtx(t, svc.pool)
	svc.SetLimit(func(context.Context, uuid.UUID, string) int64 { return 0 })
	svc.SetProvider(func(context.Context, *tenant.OrgCtx, string, string) (*Answer, error) {
		return &Answer{Body: "x"}, nil
	})
	_, _, _, err := svc.Chat(ctx, oc, uuid.Nil, "general", "hi")
	if !errors.Is(err, ErrQuota) {
		t.Fatalf("quota should stop: %v", err)
	}
	// over the limit after logging usage too
	svc.SetLimit(func(context.Context, uuid.UUID, string) int64 { return 1 })
	if _, _, _, err := svc.Chat(ctx, oc, uuid.Nil, "general", "hi"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := svc.Chat(ctx, oc, uuid.Nil, "general", "hi"); !errors.Is(err, ErrQuota) {
		t.Fatalf("second call should hit quota: %v", err)
	}
	var qe bool
	svc.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM domain_events WHERE event_type='AIQuotaExceeded' AND tenant_id=$1)`, oc.TenantID).Scan(&qe)
	if !qe {
		t.Fatal("AIQuotaExceeded not emitted")
	}
}

// TestDraftReview — spec test 2: draft → pending_review → approve →
// apply seam fires → applied + AIDraftApplied.
func TestDraftReview(t *testing.T) {
	svc, _ := testSvc(t)
	ctx := context.Background()
	oc := mkCtx(t, svc.pool)
	svc.SetDraftProvider(func(context.Context, *tenant.OrgCtx, string, string) (json.RawMessage, int, int, error) {
		return json.RawMessage(`{"title":"agenda"}`), 10, 20, nil
	})
	var applied bool
	svc.SetApply(func(context.Context, *tenant.OrgCtx, db.AiGenerations) error {
		applied = true
		return nil
	})
	gid, err := svc.Draft(ctx, oc, "agenda", "next meeting", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := svc.q.GetGeneration(ctx, db.GetGenerationParams{ID: toPg(gid), OrganizationID: toPg(oc.OrgID)})
	if g.State != "pending_review" {
		t.Fatalf("state: %s", g.State)
	}
	if err := svc.Review(ctx, oc, gid, true); err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("apply seam not called")
	}
	g, _ = svc.q.GetGeneration(ctx, db.GetGenerationParams{ID: toPg(gid), OrganizationID: toPg(oc.OrgID)})
	if g.State != "applied" {
		t.Fatalf("final state: %s", g.State)
	}
	// re-review is a CAS no-op → bad state
	if err := svc.Review(ctx, oc, gid, false); !errors.Is(err, ErrBadState) {
		t.Fatalf("re-review should fail: %v", err)
	}
	// usage was metered
	u, _ := svc.MyUsage(ctx, oc.UserID)
	if u.Requests != 1 {
		t.Fatalf("usage: %v", u.Requests)
	}
}
