// Ref: oga9-engineering/adr/ADR-024 — §25/§26/§27 org-scoped tables get the
// same forged-OrgCtx isolation guarantees as the earlier phases.
package isolation

import (
	"context"
	"testing"
	"time"

	"github.com/jedafe/oga9/internal/activities"
	"github.com/jedafe/oga9/internal/comms"
	"github.com/jedafe/oga9/internal/finance"
)

func TestFinanceFullIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := finance.NewService(e.pool, nil)

	bid, err := svc.CreateBudget(ctx, ocA, finance.BudgetIn{FiscalYear: 2027, Name: "FY27"})
	if err != nil {
		t.Fatal(err)
	}
	eid, err := svc.RequestExpense(ctx, ocA, finance.ExpenseIn{
		Title: "A spend", AmountMinor: 5000, Currency: "NGN"})
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	if b, _, err := svc.GetBudget(ctx, forged, bid); err == nil || b != nil {
		t.Fatal("forged ctx read org A budget")
	}
	if _, _, err := svc.GetExpense(ctx, forged, eid); err == nil {
		t.Fatal("forged ctx read org A expense")
	}
	if err := svc.ApproveExpense(ctx, forged, eid, ""); err == nil {
		t.Fatal("forged ctx approved org A expense")
	}
	list, err := svc.ListBudgets(ctx, forged)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Fatalf("forged ctx listed %d org A budgets", len(list))
	}
}

func TestPollsIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)
	svc := comms.NewService(e.pool)

	pid, err := svc.CreatePoll(ctx, ocA, comms.PollIn{
		Question: "Q?", Kind: "single", Options: []string{"y", "n"}})
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	if p, _, _, err := svc.GetPoll(ctx, forged, pid); err == nil || p != nil {
		t.Fatal("forged ctx read org A poll")
	}
	if err := svc.Vote(ctx, forged, pid, nil); err == nil {
		t.Fatal("forged ctx voted in org A poll")
	}
	if err := svc.OpenPoll(ctx, forged, pid); err == nil {
		t.Fatal("forged ctx opened org A poll")
	}
}

func TestVolunteersIsolation(t *testing.T) {
	e := testEnv(t)
	ctx := context.Background()
	_, ocA := mkOrg(t, e)
	orgB, ocB := mkOrg(t, e)

	aid, _, err := e.act.CreateActivity(ctx, ocA, activities.ActivityIn{
		Kind: "event", Title: "A event", StartsAt: time.Now().Add(72 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	rid, err := e.act.CreateVolunteerRole(ctx, ocA, aid, activities.VolunteerRoleIn{Name: "usher", Slots: 2})
	if err != nil {
		t.Fatal(err)
	}
	forged := forge(ocA, orgB, ocB.TenantID)
	// org B ctx can't see org A roles/signups or act on them
	roles, err := e.act.VolunteerRoles(ctx, forged, aid)
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 0 {
		t.Fatalf("forged ctx listed %d org A volunteer roles", len(roles))
	}
	if _, err := e.act.Signup(ctx, forged, aid, rid, ""); err == nil {
		t.Fatal("forged ctx signed up to org A role")
	}
	if err := e.act.DeleteVolunteerRole(ctx, forged, rid); err == nil {
		t.Fatal("forged ctx deleted org A role")
	}
}
