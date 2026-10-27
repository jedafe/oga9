// Ref: oga9-engineering/adr/ADR-024 — every table carrying a scope column
// must land an isolation test in the same change. Post-MVP modules
// (§16–§23) ship org-scoped tables; forged OrgCtx must not read org A's
// rows nor write into org B under org A's membership.
package isolation

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/ai"
	"github.com/jedafe/oga9/internal/analytics"
	"github.com/jedafe/oga9/internal/assets"
	"github.com/jedafe/oga9/internal/elections"
	"github.com/jedafe/oga9/internal/governance"
	"github.com/jedafe/oga9/internal/integrations"
	"github.com/jedafe/oga9/internal/projects"
	"github.com/jedafe/oga9/internal/welfare"
)

func TestGovernanceIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := governance.NewService(e.pool)

	mid, err := svc.CreateMotion(ctx, ocA, uuid.Nil, uuid.Nil, "A motion", "body")
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	list, err := svc.ListMotions(ctx, forged, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A motions", len(list))
	}
	if _, err := svc.RecordVote(ctx, forged, mid, "passed", 1, 0, 0); !errors.Is(err, governance.ErrNotFound) {
		t.Fatalf("forged ctx voted org A motion: %v", err)
	}
}

func TestWelfareIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := welfare.NewService(e.pool)

	if _, err := e.pool.Exec(ctx,
		`INSERT INTO welfare_cases (organization_id, membership_id, kind, title, state)
		 VALUES ($1,$2,'medical','A case','requested')`, orgA, ocA.MembershipID); err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	list, err := svc.ListCases(ctx, forged, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A welfare cases", len(list))
	}
	var cid uuid.UUID
	if err := e.pool.QueryRow(ctx,
		`SELECT id FROM welfare_cases WHERE organization_id=$1 LIMIT 1`, orgA).Scan(&cid); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetCase(ctx, forged, cid); err == nil {
		t.Fatal("forged ctx read an org A welfare case")
	}
}

func TestAnalyticsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := analytics.NewService(e.pool)

	if _, err := e.pool.Exec(ctx,
		`INSERT INTO report_runs (organization_id, kind, period) VALUES ($1,'monthly','2026-10')`,
		orgA); err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	runs, err := svc.ListRuns(ctx, forged)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 0 {
		t.Fatalf("forged ctx listed %d org A report runs", len(runs))
	}
}

func TestAssetsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := assets.NewService(e.pool)

	aid, err := svc.Register(ctx, ocA, "equipment", "A's projector", "", "", "", time.Time{}, 0, "NGN", uuid.Nil)
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	if _, err := svc.Get(ctx, forged, aid); !errors.Is(err, assets.ErrNotFound) {
		t.Fatalf("forged ctx read org A asset: %v", err)
	}
	list, err := svc.List(ctx, forged, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A assets", len(list))
	}
}

func TestProjectsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := projects.NewService(e.pool)

	pid, err := svc.Propose(ctx, ocA, "A project", "", uuid.Nil, time.Time{}, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	if _, err := svc.Get(ctx, forged, pid); err == nil {
		t.Fatal("forged ctx read org A project")
	}
	list, err := svc.List(ctx, forged, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A projects", len(list))
	}
}

func TestElectionsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := elections.NewService(e.pool)

	var termID uuid.UUID
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO terms (organization_id, label, starts_at, ends_at)
		 VALUES ($1,'T1',now(),now()+interval '1 year') RETURNING id`, orgA).Scan(&termID); err != nil {
		t.Fatal(err)
	}
	eid, err := svc.Create(ctx, ocA, termID, "A election", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	if _, err := svc.Get(ctx, forged, eid); err == nil {
		t.Fatal("forged ctx read org A election")
	}
	list, err := svc.List(ctx, forged)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A elections", len(list))
	}
	// forged vote: org A's membership is not a member of org B's election
	if err := svc.Vote(ctx, forged, eid, nil); err == nil {
		t.Fatal("forged ctx voted in a foreign election")
	}
}

func TestIntegrationsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := integrations.NewService(e.pool)

	if _, err := e.pool.Exec(ctx,
		`INSERT INTO connectors (organization_id, kind, config, credential_ref) VALUES ($1,'japaconnect','{}','env:X')`,
		orgA); err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	list, err := svc.ListConnectors(ctx, forged)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A connectors", len(list))
	}
}

func TestAIIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	orgA, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := ai.NewService(e.pool)

	var conv uuid.UUID
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO ai_conversations (organization_id, user_id, persona) VALUES ($1,$2,'general') RETURNING id`,
		orgA, ocA.UserID).Scan(&conv); err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	list, err := svc.Conversations(ctx, forged)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A conversations", len(list))
	}
	if _, err := svc.Messages(ctx, forged, conv); err == nil {
		t.Fatal("forged ctx read org A conversation messages")
	}
}
