package tenant

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/db"
)

// §03 scope filter: a branch-scoped appointment confers only the _branch
// variant of its position's nodes — never the unsuffixed base node.
// Committee membership confers _committee on base nodes already held.
func TestScopedAppointmentResolution(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "own"+uuid.NewString()[:8]+"@test.oga9")
	memberEmail := "mbr" + uuid.NewString()[:8] + "@test.oga9"
	member := mkUser(t, pool, memberEmail)

	org, err := svc.CreateOrg(ctx, owner, "Scope Union", "sc"+uuid.NewString()[:6], "union", "union", "NG", "Lagos", "o@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	oc, err := svc.ResolveOrgCtx(ctx, owner, org.ID)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := svc.CreateInvitation(ctx, oc, memberEmail, "", "", "floor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, member, inv.Token); err != nil {
		t.Fatal(err)
	}
	m, err := db.New(pool).GetMembership(ctx, db.GetMembershipParams{OrganizationID: toPg(org.ID), UserID: toPg(member)})
	if err != nil {
		t.Fatal(err)
	}
	memberID := fromPg(m.ID)

	pos, err := svc.CreatePosition(ctx, oc, "Branch Treasurer", "branch-treasurer", true, 1, []string{"org.finance.payments.record"})
	if err != nil {
		t.Fatal(err)
	}
	pos2, err := svc.CreatePosition(ctx, oc, "Auditor", "auditor", true, 1, []string{"org.finance.reports.read"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	termID, err := svc.CreateTerm(ctx, oc, "2026-2028", now.AddDate(0, 0, -1), now.AddDate(2, 0, 0), true)
	if err != nil {
		t.Fatal(err)
	}
	branch, err := svc.CreateUnit(ctx, oc, uuid.Nil, "branch", "ikeja", "Ikeja Branch", "ikeja", nil)
	if err != nil {
		t.Fatal(err)
	}

	// lifecycle guard: an 'applied' member can't hold office
	if _, err := svc.AppointOfficer(ctx, oc, termID, pos, memberID, branch, uuid.Nil, now); err != ErrMemberNotActive {
		t.Fatalf("applied member should be rejected, got %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE organization_memberships SET lifecycle_state='active' WHERE id=$1", memberID); err != nil {
		t.Fatal(err)
	}

	// out-of-window start is rejected against the term's dates
	if _, err := svc.AppointOfficer(ctx, oc, termID, pos, memberID, branch, uuid.Nil, now.AddDate(0, 0, -60)); err != ErrBadValue {
		t.Fatalf("pre-term start should be rejected, got %v", err)
	}

	_, err = svc.AppointOfficer(ctx, oc, termID, pos, memberID, branch, uuid.Nil, now)
	if err != nil {
		t.Fatalf("scoped appoint: %v", err)
	}
	// scoped seat: _branch variant resolves, the base node must not
	if svc.HasPermission(ctx, member, org.ID, "org.finance.payments.record") {
		t.Fatal("branch-scoped grant leaked the unsuffixed node")
	}
	if !svc.HasPermission(ctx, member, org.ID, "org.finance.payments.record_branch") {
		t.Fatal("branch-scoped grant missing _branch variant")
	}

	// unscoped appointment on another position grants the base node
	if _, err := svc.AppointOfficer(ctx, oc, termID, pos2, memberID, uuid.Nil, uuid.Nil, now); err != nil {
		t.Fatalf("unscoped appoint: %v", err)
	}
	if !svc.HasPermission(ctx, member, org.ID, "org.finance.reports.read") {
		t.Fatal("unscoped position grant should confer the base node")
	}

	// committee membership confers _committee on base nodes already held
	// (member role grants org.members.view).
	if svc.HasPermission(ctx, member, org.ID, "org.members.view_committee") {
		t.Fatal("_committee resolved before seating")
	}
	cu, err := svc.CreateUnit(ctx, oc, uuid.Nil, "committee", "", "Audit Committee", "audit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.AddCommitteeMember(ctx, oc, cu, memberID, "member"); err != nil {
		t.Fatal(err)
	}
	if !svc.HasPermission(ctx, member, org.ID, "org.members.view_committee") {
		t.Fatal("committee seat should confer _committee on held base nodes")
	}

	// a future-dated appointment inside the term window is allowed —
	// term close must land ends_at >= starts_at (GREATEST guard).
	pos3, err := svc.CreatePosition(ctx, oc, "Incoming Secretary", "incoming-secretary", true, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	future := now.AddDate(0, 0, 30) // inside the term window
	apID, err := svc.AppointOfficer(ctx, oc, termID, pos3, memberID, uuid.Nil, uuid.Nil, future)
	if err != nil {
		t.Fatalf("future-dated appoint: %v", err)
	}

	// term close ends appointments; GREATEST keeps ends_at >= starts_at for
	// the future-dated seat (never ends<starts).
	if err := svc.CloseTerm(ctx, oc, termID); err != nil {
		t.Fatal(err)
	}
	var endsAt time.Time
	if err := pool.QueryRow(ctx, "SELECT ends_at FROM officer_appointments WHERE id=$1", apID).Scan(&endsAt); err != nil {
		t.Fatal(err)
	}
	if endsAt.Before(future.Truncate(24 * time.Hour)) {
		t.Fatalf("ends_at %v precedes future starts_at", endsAt)
	}
	if svc.HasPermission(ctx, member, org.ID, "org.finance.payments.record_branch") {
		t.Fatal("scoped grant should drop with the term")
	}
}
