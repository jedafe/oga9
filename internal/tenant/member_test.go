package tenant

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
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
	lone, err := svc.Apply(ctx, org.ID, late, "late@t.co", "", nil, uuid.Nil)
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
	appID, err := svc.Apply(ctx, org.ID, applicant, "applicant@t.co", "", json.RawMessage(`{"village":"Ota"}`), oc.MembershipID)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// open-application dedupe
	if _, err := svc.Apply(ctx, org.ID, applicant, "applicant@t.co", "", nil, uuid.Nil); err != ErrAlreadyApplied {
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
	rid, err := svc.RenewMember(ctx, oc, mid, "2026", time.Now(), time.Now().AddDate(1, 0, 0))
	if err != nil || rid == uuid.Nil {
		t.Fatalf("renew: %v", err)
	}
	if _, err := svc.RenewMember(ctx, oc, mid, "2026", time.Now(), time.Now().AddDate(1, 0, 0)); err != ErrDuplicateRenewal {
		t.Fatalf("dup period renewal should 409, got %v", err)
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
}
