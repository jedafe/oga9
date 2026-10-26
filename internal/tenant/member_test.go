package tenant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/db"
)

// §11 flows: application → screen → approve (member_no + sponsor) →
// register → activate → suspend/reinstate → card → renew → terminal
// teardown — plus sponsor_required, invalid transitions, import, history.
func TestMemberLifecycleFlow(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "ow"+uuid.NewString()[:8]+"@test.oga9")
	applicant := mkUser(t, pool, "ap"+uuid.NewString()[:8]+"@test.oga9")
	late := mkUser(t, pool, "lt"+uuid.NewString()[:8]+"@test.oga9")

	org, err := svc.CreateOrg(ctx, owner, "Member Union", "mu"+uuid.NewString()[:6], "union", "union", "NG", "Lagos", "o@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	oc, err := svc.ResolveOrgCtx(ctx, owner, org.ID)
	if err != nil {
		t.Fatal(err)
	}

	// sponsor_required=1 is enforced at APPROVE, not apply — intake stays
	// open; the unsponsored application screens fine then fails the guard.
	if err := svc.SetSetting(ctx, oc, "org.membership.sponsor_required", json.RawMessage("1")); err != nil {
		t.Fatal(err)
	}
	lone, err := svc.Apply(ctx, org.ID, late, "late@t.co", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, lone, "screen", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, lone, "approve", nil); err != ErrSponsorRequired {
		t.Fatalf("unsponsored approve under required=1 should fail, got %v", err)
	}
	if err := svc.SetSetting(ctx, oc, "org.membership.sponsor_required", json.RawMessage("0")); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, lone, "approve", nil); err != nil {
		t.Fatalf("approve after setting drop: %v", err)
	}

	// owner's membership is the sponsor (creator lands 'active')
	appID, err := svc.Apply(ctx, org.ID, applicant, "applicant@t.co", "", json.RawMessage(`{"village":"Ota"}`), []uuid.UUID{oc.MembershipID})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// open-application dedupe
	if _, err := svc.Apply(ctx, org.ID, applicant, "applicant@t.co", "", nil, nil); err != ErrAlreadyApplied {
		t.Fatalf("second open application should be deduped, got %v", err)
	}

	// decision flow: applied→approved skips screening — §11 intake is
	// applied→screened→approved, so the jump is genuinely invalid.
	if err := svc.DecideApplication(ctx, oc, appID, "approve", nil); err != ErrInvalidTransition {
		t.Fatalf("applied→approved skip should be invalid, got %v", err)
	}
	if err := svc.DecideApplication(ctx, oc, appID, "screen", nil); err != nil {
		t.Fatalf("screen: %v", err)
	}
	if err := svc.DecideApplication(ctx, oc, appID, "approve", nil); err != nil {
		t.Fatalf("approve: %v", err)
	}
	mb, err := pool.Query(ctx, "SELECT id, member_no, lifecycle_state FROM organization_memberships WHERE organization_id=$1 AND user_id=$2", org.ID, applicant)
	if err != nil {
		t.Fatal(err)
	}
	defer mb.Close()
	var mid uuid.UUID
	var memberNo, state string
	for mb.Next() {
		if err := mb.Scan(&mid, &memberNo, &state); err != nil {
			t.Fatal(err)
		}
	}
	if mid == uuid.Nil || state != "approved" || memberNo == "" {
		t.Fatalf("expected approved membership with member_no, got %v %q %q", mid, memberNo, state)
	}
	// member_no format: {SLUG}-{YYYY}-{seq5} — org slug upper-cased.
	if !strings.HasPrefix(memberNo, "MU") || !strings.Contains(memberNo, "-") {
		t.Fatalf("member_no looks wrong: %q", memberNo)
	}
	var sponsors int
	pool.QueryRow(ctx, "SELECT count(*) FROM member_sponsors WHERE membership_id=$1", mid).Scan(&sponsors)
	if sponsors != 1 {
		t.Fatalf("expected sponsor link, got %d", sponsors)
	}

	// invalid transition: approved→active must skip register first
	if err := svc.TransitionMember(ctx, oc, mid, "activate"); err != ErrInvalidTransition {
		t.Fatalf("approved→active should be invalid, got %v", err)
	}
	if err := svc.TransitionMember(ctx, oc, mid, "register"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.TransitionMember(ctx, oc, mid, "activate"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !svc.HasPermission(ctx, applicant, org.ID, "org.members.view") {
		t.Fatal("active member should hold member-role grants")
	}
	// joined_at stamps on activation
	var joined bool
	pool.QueryRow(ctx, "SELECT joined_at IS NOT NULL FROM organization_memberships WHERE id=$1", mid).Scan(&joined)
	if !joined {
		t.Fatal("joined_at not stamped on activation")
	}

	// suspension blocks the resolved grant set (ADR-014) + cache invalidation
	if err := svc.TransitionMember(ctx, oc, mid, "suspend"); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if svc.HasPermission(ctx, applicant, org.ID, "org.members.view") {
		t.Fatal("suspended member must lose resolved grants")
	}
	if err := svc.TransitionMember(ctx, oc, mid, "reinstate"); err != nil {
		t.Fatalf("reinstate: %v", err)
	}
	if !svc.HasPermission(ctx, applicant, org.ID, "org.members.view") {
		t.Fatal("reinstated member should hold grants again")
	}

	// card + renewal
	card, err := svc.IssueCard(ctx, oc, mid, nil)
	if err != nil {
		t.Fatalf("card: %v", err)
	}
	if card.Serial == "" {
		t.Fatal("empty card serial")
	}
	// one active card per member — re-issue returns the live card
	if card2, err := svc.IssueCard(ctx, oc, mid, nil); err != nil || card2.Serial != card.Serial {
		t.Fatalf("re-issue should return the live card, got %v err %v", card2.Serial, err)
	}
	rid, err := svc.RenewMember(ctx, oc, mid, "2026", time.Now(), time.Now().AddDate(1, 0, 0), uuid.Nil)
	if err != nil || rid == uuid.Nil {
		t.Fatalf("renew: %v", err)
	}
	if _, err := svc.RenewMember(ctx, oc, mid, "2026", time.Now(), time.Now().AddDate(1, 0, 0), uuid.Nil); err != ErrDuplicateRenewal {
		t.Fatalf("dup period renewal should 409, got %v", err)
	}
	if _, err := svc.RenewMember(ctx, oc, mid, "2025", time.Now(), time.Now().AddDate(-1, 0, 0), uuid.Nil); err != ErrBadValue {
		t.Fatalf("backwards renewal window should fail, got %v", err)
	}
	if !Eligible("active", "good") || Eligible("suspended", "good") || Eligible("active", "bad") {
		t.Fatal("eligibility predicate wrong")
	}

	// history + me/memberships
	hist, err := svc.MemberHistory(ctx, oc, mid)
	if err != nil || len(hist) < 3 {
		t.Fatalf("expected journey events, got %d err %v", len(hist), err)
	}
	mine, err := svc.MyMemberships(ctx, applicant)
	if err != nil || len(mine) != 1 {
		t.Fatalf("my memberships: %d err %v", len(mine), err)
	}

	// unit scope + directory filter: patch the member into the branch unit,
	// then the ?unit= filter should scope the directory.
	br2, err := svc.CreateUnit(ctx, oc, uuid.Nil, "branch", "vi", "VI Branch", "vi", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.PatchMember(ctx, oc, mid, "", br2); err != nil {
		t.Fatalf("scope patch: %v", err)
	}
	if rows, err := svc.Members(ctx, oc, "", "", br2); err != nil || len(rows) != 1 {
		t.Fatalf("unit filter should return exactly the scoped member, got %d err %v", len(rows), err)
	}
	if rows, err := svc.Members(ctx, oc, "", "", uuid.Nil); err != nil || len(rows) < 2 {
		t.Fatalf("unfiltered directory should return all members, got %d err %v", len(rows), err)
	}

	// self-resign: a member resigns their own membership without the
	// manage node (service-level; handler gates mid==oc.MembershipID).
	resigner := mkUser(t, pool, "rs"+uuid.NewString()[:8]+"@test.oga9")
	rapp, err := svc.Apply(ctx, org.ID, resigner, "rs@t.co", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, rapp, "screen", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, rapp, "approve", nil); err != nil {
		t.Fatal(err)
	}
	var rmid pgtype.UUID
	pool.QueryRow(ctx, "SELECT id FROM organization_memberships WHERE organization_id=$1 AND user_id=$2", org.ID, resigner).Scan(&rmid)
	rctx := &OrgCtx{OrgID: org.ID, UserID: resigner, MembershipID: fromPg(rmid), TenantID: org.TenantID}
	if _, err := pool.Exec(ctx, "UPDATE organization_memberships SET lifecycle_state='active' WHERE id=$1", rmid); err != nil {
		t.Fatal(err)
	}
	if err := svc.TransitionMember(ctx, rctx, fromPg(rmid), "resign"); err != nil {
		t.Fatalf("self-resign: %v", err)
	}
	var rst string
	pool.QueryRow(ctx, "SELECT lifecycle_state FROM organization_memberships WHERE id=$1", rmid).Scan(&rst)
	if rst != "resigned" {
		t.Fatalf("expected resigned, got %s", rst)
	}

	// deceased: terminal teardown — card revoked, member history kept
	if err := svc.TransitionMember(ctx, oc, mid, "deceased"); err != nil {
		t.Fatalf("deceased: %v", err)
	}
	var revoked bool
	pool.QueryRow(ctx, "SELECT revoked_at IS NOT NULL FROM membership_cards WHERE id=$1", card.ID).Scan(&revoked)
	if !revoked {
		t.Fatal("deceased member's card not revoked")
	}
	if svc.HasPermission(ctx, applicant, org.ID, "org.members.view") {
		t.Fatal("deceased member must not resolve grants")
	}
	// terminal: no further transitions
	if err := svc.TransitionMember(ctx, oc, mid, "reinstate"); err != ErrInvalidTransition {
		t.Fatalf("deceased→active should be invalid, got %v", err)
	}
}

// Import: rows → applications (never direct members), per-row errors.
func TestMemberImport(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "im"+uuid.NewString()[:8]+"@test.oga9")
	org, err := svc.CreateOrg(ctx, owner, "Import Union", "iu"+uuid.NewString()[:6], "union", "union", "NG", "Kano", "i@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := svc.ResolveOrgCtx(ctx, owner, org.ID)
	rows, _ := json.Marshal([]map[string]any{
		{"email": "a@t.co", "phone": "0801"},
		{"phone": "0802"},
		{"email": "", "phone": ""}, // bad row — no contact channel
	})
	impID, err := svc.CreateImport(ctx, oc, "members.csv", rows)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ProcessImport(ctx, impID); err != nil {
		t.Fatalf("process: %v", err)
	}
	var okN, errN int
	var status string
	pool.QueryRow(ctx, "SELECT status, ok_count, err_count FROM member_imports WHERE id=$1", impID).Scan(&status, &okN, &errN)
	if status != "done" || okN != 2 || errN != 1 {
		t.Fatalf("import result %s ok=%d err=%d", status, okN, errN)
	}
	var apps int
	pool.QueryRow(ctx, "SELECT count(*) FROM membership_applications WHERE organization_id=$1", org.ID).Scan(&apps)
	if apps != 2 {
		t.Fatalf("expected 2 applications, got %d", apps)
	}
	// imports create applications, never memberships
	var mems int
	pool.QueryRow(ctx, "SELECT count(*) FROM organization_memberships WHERE organization_id=$1 AND user_id IS NOT NULL", org.ID).Scan(&mems)
	if mems != 1 { // only the owner
		t.Fatalf("imports must not create members directly, got %d", mems)
	}
	// idempotent replay: processing the same import again creates nothing
	if err := svc.ProcessImport(ctx, impID); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM membership_applications WHERE organization_id=$1", org.ID).Scan(&apps)
	if apps != 2 {
		t.Fatalf("replayed import minted duplicates, got %d", apps)
	}
}

// sponsor_required=2 + self-withdrawal + multi-sponsor staging.
func TestSponsorWorkflow(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "sw"+uuid.NewString()[:8]+"@test.oga9")
	sp2user := mkUser(t, pool, "s2"+uuid.NewString()[:8]+"@test.oga9")
	cand := mkUser(t, pool, "cd"+uuid.NewString()[:8]+"@test.oga9")
	wd := mkUser(t, pool, "wd"+uuid.NewString()[:8]+"@test.oga9")

	org, err := svc.CreateOrg(ctx, owner, "Sponsor Union", "su"+uuid.NewString()[:6], "union", "union", "NG", "Ibadan", "s@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := svc.ResolveOrgCtx(ctx, owner, org.ID)
	if err := svc.SetSetting(ctx, oc, "org.membership.sponsor_required", json.RawMessage("2")); err != nil {
		t.Fatal(err)
	}
	// bring sp2user in as a second active member (sponsor #2)
	sp2Email := "s2" + uuid.NewString()[:8] + "@test.oga9"
	sp2user = mkUser(t, pool, sp2Email)
	inv, err := svc.CreateInvitation(ctx, oc, sp2Email, "", "", "floor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, sp2user, inv.Token); err != nil {
		t.Fatal(err)
	}
	sp2, err := db.New(pool).GetMembership(ctx, db.GetMembershipParams{OrganizationID: toPg(org.ID), UserID: toPg(sp2user)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "UPDATE organization_memberships SET lifecycle_state='active' WHERE id=$1", sp2.ID); err != nil {
		t.Fatal(err)
	}

	// one sponsor under required=2 → approve fails
	one, err := svc.Apply(ctx, org.ID, cand, "cand@t.co", "", nil, []uuid.UUID{oc.MembershipID})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, one, "screen", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, one, "approve", nil); err != ErrSponsorRequired {
		t.Fatalf("1 of 2 sponsors should fail approve, got %v", err)
	}
	if err := svc.DecideApplication(ctx, oc, one, "withdraw", nil); err != nil {
		t.Fatalf("withdraw the rejected app: %v", err)
	}

	// two sponsors (primary + staged extra) → approve succeeds and both
	// sponsor links migrate onto the membership.
	two, err := svc.Apply(ctx, org.ID, cand, "cand2@t.co", "", nil, []uuid.UUID{oc.MembershipID, fromPg(sp2.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, two, "screen", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, two, "approve", nil); err != nil {
		t.Fatalf("2-sponsor approve: %v", err)
	}
	var n int64
	pool.QueryRow(ctx,
		"SELECT count(*) FROM member_sponsors ms JOIN organization_memberships m ON m.id=ms.membership_id WHERE m.user_id=$1 AND m.organization_id=$2",
		cand, org.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("expected 2 membership sponsors after approve, got %d", n)
	}

	// decide-time sponsors: a public-style application (no sponsors at
	// intake) in a sponsor-required org gets them attached by the screener
	// at approve — otherwise the queue deadlocks for public applicants.
	publicApp, err := svc.Apply(ctx, org.ID, wd, "pub@t.co", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, publicApp, "screen", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, publicApp, "approve", nil); err != ErrSponsorRequired {
		t.Fatalf("no-sponsor approve under required=2 should fail, got %v", err)
	}
	if err := svc.DecideApplication(ctx, oc, publicApp, "approve",
		[]uuid.UUID{oc.MembershipID, fromPg(sp2.ID)}); err != nil {
		t.Fatalf("decide-time sponsors should satisfy the count, got %v", err)
	}
	var pn int64
	pool.QueryRow(ctx,
		"SELECT count(*) FROM member_sponsors ms JOIN membership_applications a ON a.id=$1 WHERE ms.application_id=$1",
		publicApp).Scan(&pn)
	if pn != 0 {
		t.Fatalf("staged sponsors should have moved to the membership, %d still staged", pn)
	}

	// self-withdraw: the applicant can withdraw their own open application;
	// a stranger can't, and an approved one can't anymore.
	wd2 := mkUser(t, pool, "w2"+uuid.NewString()[:8]+"@test.oga9")
	wa, err := svc.Apply(ctx, org.ID, wd2, "w2@t.co", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WithdrawApplication(ctx, cand, org.ID, wa); err != ErrApplicationNotFound {
		t.Fatalf("foreign withdraw should be not_found, got %v", err)
	}
	if err := svc.WithdrawApplication(ctx, wd2, org.ID, wa); err != nil {
		t.Fatalf("own withdraw: %v", err)
	}
	var st string
	pool.QueryRow(ctx, "SELECT status FROM membership_applications WHERE id=$1", wa).Scan(&st)
	if st != "withdrawn" {
		t.Fatalf("expected withdrawn, got %s", st)
	}
}

// Regression: invited members land 'applied' with no application row —
// the membership-level intake path must carry them to activation, and a
// pre-signup approved application must materialize when the account binds.
func TestIntakeDeadEnds(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "io"+uuid.NewString()[:8]+"@test.oga9")

	org, err := svc.CreateOrg(ctx, owner, "Intake Union", "iu"+uuid.NewString()[:6], "union", "union", "NG", "Abuja", "i@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := svc.ResolveOrgCtx(ctx, owner, org.ID)

	// 1) invite → accept → 'applied' → approve (mints member_no) →
	//    register → activate. Previously 'applied' was a dead end.
	invEmail := "iv" + uuid.NewString()[:8] + "@test.oga9"
	invUser := mkUser(t, pool, invEmail)
	inv, err := svc.CreateInvitation(ctx, oc, invEmail, "", "", "floor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, invUser, inv.Token); err != nil {
		t.Fatalf("accept: %v", err)
	}
	moc, err := svc.ResolveOrgCtx(ctx, invUser, org.ID)
	if err != nil {
		t.Fatalf("invited member ctx: %v", err)
	}
	// intake sequence is the same as the application path: applied→screened→approved
	if err := svc.TransitionMember(ctx, oc, moc.MembershipID, "approve"); err != ErrInvalidTransition {
		t.Fatalf("skipping screen should be invalid, got %v", err)
	}
	if err := svc.TransitionMember(ctx, oc, moc.MembershipID, "screen"); err != nil {
		t.Fatalf("membership screen: %v", err)
	}
	if err := svc.TransitionMember(ctx, oc, moc.MembershipID, "approve"); err != nil {
		t.Fatalf("membership approve: %v", err)
	}
	var memberNo string
	var state string
	pool.QueryRow(ctx, "SELECT member_no, lifecycle_state FROM organization_memberships WHERE id=$1", moc.MembershipID).
		Scan(&memberNo, &state)
	if state != "approved" || memberNo == "" {
		t.Fatalf("invited approve should mint member_no, got %q %q", memberNo, state)
	}
	if err := svc.TransitionMember(ctx, oc, moc.MembershipID, "register"); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := svc.TransitionMember(ctx, oc, moc.MembershipID, "activate"); err != nil {
		t.Fatalf("activate: %v", err)
	}
	pool.QueryRow(ctx, "SELECT lifecycle_state FROM organization_memberships WHERE id=$1", moc.MembershipID).Scan(&state)
	if state != "active" {
		t.Fatalf("invited member never reached active, got %q", state)
	}

	// 2) public (pre-signup) application → approve → materialize on bind.
	pubEmail := "pb" + uuid.NewString()[:8] + "@test.oga9"
	pubUser := mkUser(t, pool, pubEmail)
	app, err := svc.Apply(ctx, org.ID, uuid.Nil, pubEmail, "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, app, "screen", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, app, "approve", nil); err != nil {
		t.Fatalf("pre-signup approve: %v", err)
	}
	var n int64
	pool.QueryRow(ctx, "SELECT count(*) FROM organization_memberships WHERE organization_id=$1 AND user_id=$2", org.ID, pubUser).Scan(&n)
	if n != 0 {
		t.Fatal("pre-signup approve should not materialize a membership yet")
	}
	if err := svc.MaterializeApprovedApplications(ctx, pubUser); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	pool.QueryRow(ctx, "SELECT lifecycle_state FROM organization_memberships WHERE organization_id=$1 AND user_id=$2", org.ID, pubUser).Scan(&state)
	if state != "approved" {
		t.Fatalf("materialized membership should be 'approved', got %q", state)
	}
	// idempotent: a second run must not mint a second membership
	if err := svc.MaterializeApprovedApplications(ctx, pubUser); err != nil {
		t.Fatal(err)
	}
	pool.QueryRow(ctx, "SELECT count(*) FROM organization_memberships WHERE organization_id=$1 AND user_id=$2", org.ID, pubUser).Scan(&n)
	if n != 1 {
		t.Fatalf("materialize replay minted %d memberships", n)
	}

	// 3) sponsor double-count: required=2, attaching the primary sponsor
	// as a screener-extra must NOT satisfy the count with one person.
	if err := svc.SetSetting(ctx, oc, "org.membership.sponsor_required", json.RawMessage("2")); err != nil {
		t.Fatal(err)
	}
	dbl, err := svc.Apply(ctx, org.ID, uuid.Nil, "dbl@t.co", "", nil, []uuid.UUID{oc.MembershipID})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, dbl, "screen", nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, dbl, "approve", []uuid.UUID{oc.MembershipID}); err != ErrSponsorRequired {
		t.Fatalf("primary-as-extra must not count twice, got %v", err)
	}
}
