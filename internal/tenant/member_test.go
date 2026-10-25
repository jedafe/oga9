package tenant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

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
	if err := svc.DecideApplication(ctx, oc, lone, "screen"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, lone, "approve"); err != ErrSponsorRequired {
		t.Fatalf("unsponsored approve under required=1 should fail, got %v", err)
	}
	if err := svc.SetSetting(ctx, oc, "org.membership.sponsor_required", json.RawMessage("0")); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, lone, "approve"); err != nil {
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
	if err := svc.DecideApplication(ctx, oc, appID, "approve"); err != ErrInvalidTransition {
		t.Fatalf("applied→approved skip should be invalid, got %v", err)
	}
	if err := svc.DecideApplication(ctx, oc, appID, "screen"); err != nil {
		t.Fatalf("screen: %v", err)
	}
	if err := svc.DecideApplication(ctx, oc, appID, "approve"); err != nil {
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
	rid, err := svc.RenewMember(ctx, oc, mid, "2026", time.Now(), time.Now().AddDate(1, 0, 0))
	if err != nil || rid == uuid.Nil {
		t.Fatalf("renew: %v", err)
	}
	if _, err := svc.RenewMember(ctx, oc, mid, "2026", time.Now(), time.Now().AddDate(1, 0, 0)); err != ErrDuplicateRenewal {
		t.Fatalf("dup period renewal should 409, got %v", err)
	}
	if _, err := svc.RenewMember(ctx, oc, mid, "2025", time.Now(), time.Now().AddDate(-1, 0, 0)); err != ErrBadValue {
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
	if err := svc.DecideApplication(ctx, oc, one, "screen"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, one, "approve"); err != ErrSponsorRequired {
		t.Fatalf("1 of 2 sponsors should fail approve, got %v", err)
	}
	if err := svc.DecideApplication(ctx, oc, one, "withdraw"); err != nil {
		t.Fatalf("withdraw the rejected app: %v", err)
	}

	// two sponsors (primary + staged extra) → approve succeeds and both
	// sponsor links migrate onto the membership.
	two, err := svc.Apply(ctx, org.ID, cand, "cand2@t.co", "", nil, []uuid.UUID{oc.MembershipID, fromPg(sp2.ID)})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, two, "screen"); err != nil {
		t.Fatal(err)
	}
	if err := svc.DecideApplication(ctx, oc, two, "approve"); err != nil {
		t.Fatalf("2-sponsor approve: %v", err)
	}
	var n int64
	pool.QueryRow(ctx,
		"SELECT count(*) FROM member_sponsors ms JOIN organization_memberships m ON m.id=ms.membership_id WHERE m.user_id=$1 AND m.organization_id=$2",
		cand, org.ID).Scan(&n)
	if n != 2 {
		t.Fatalf("expected 2 membership sponsors after approve, got %d", n)
	}

	// self-withdraw: the applicant can withdraw their own open application;
	// a stranger can't, and an approved one can't anymore.
	wa, err := svc.Apply(ctx, org.ID, wd, "wd@t.co", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.WithdrawApplication(ctx, cand, org.ID, wa); err != ErrApplicationNotFound {
		t.Fatalf("foreign withdraw should be not_found, got %v", err)
	}
	if err := svc.WithdrawApplication(ctx, wd, org.ID, wa); err != nil {
		t.Fatalf("own withdraw: %v", err)
	}
	var st string
	pool.QueryRow(ctx, "SELECT status FROM membership_applications WHERE id=$1", wa).Scan(&st)
	if st != "withdrawn" {
		t.Fatalf("expected withdrawn, got %s", st)
	}
}
