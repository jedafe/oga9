// Ref: oga9-engineering/sections/13-events-meetings.md test scenarios —
// notice-window warning, bulk attendance + fines, minutes versioning,
// overdue items, capacity + check-in, recurrence expansion.
package activities

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/finance"
	"github.com/jedafe/oga9/internal/tenant"
)

func testEnv(t *testing.T) (*Service, *tenant.Service, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("OGA9_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("OGA9_TEST_DATABASE_URL not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Skip("test db unavailable")
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Skip("test db unavailable: ", err)
	}
	addr := os.Getenv("OGA9_TEST_VALKEY_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	vk, err := valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}})
	if err != nil {
		t.Skip("valkey unavailable: ", err)
	}
	svc := NewService(pool)
	fin := finance.NewService(pool, vk)
	org := tenant.NewService(pool, vk)
	svc.SetFineHook(fin.AssessFine)
	return svc, org, pool
}

func mkOrgMember(t *testing.T, org *tenant.Service, pool *pgxpool.Pool, email string) (uuid.UUID, *tenant.OrgCtx, uuid.UUID) {
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name, password_hash, status) VALUES ($1,'u','x','active')
		 ON CONFLICT (email) DO UPDATE SET email=$1 RETURNING id`, email).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	o, err := org.CreateOrg(ctx, uid, "Act Union", "au"+uuid.NewString()[:6], "union", "union", "NG", "Jos", "a@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := org.ResolveOrgCtx(ctx, uid, o.ID)
	return uid, oc, o.ID
}

func addMember(t *testing.T, pool *pgxpool.Pool, orgID uuid.UUID, email string) uuid.UUID {
	ctx := context.Background()
	var uid uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name, password_hash, status) VALUES ($1,'u','x','active')
		 ON CONFLICT (email) DO UPDATE SET email=$1 RETURNING id`, email).Scan(&uid); err != nil {
		t.Fatal(err)
	}
	var mid uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, member_no, lifecycle_state)
		 VALUES ($1,$2,$3,'active') RETURNING id`,
		orgID, uid, "M-"+uuid.NewString()[:6]).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	return mid
}

func TestActivitiesFlow(t *testing.T) {
	svc, orgSvc, pool := testEnv(t)
	ctx := context.Background()
	_, oc, orgID := mkOrgMember(t, orgSvc, pool, "act-own-"+uuid.NewString()[:6]+"@t.co")
	m1 := addMember(t, pool, orgID, "act-m1-"+uuid.NewString()[:6]+"@t.co")
	m2 := addMember(t, pool, orgID, "act-m2-"+uuid.NewString()[:6]+"@t.co")

	// notice rule: meeting inside the notice window warns, doesn't block
	if _, err := pool.Exec(ctx,
		`INSERT INTO org_settings (organization_id, key, value) VALUES ($1,'org.meeting.notice_days','7')
		 ON CONFLICT (organization_id, key) DO UPDATE SET value='7'`, orgID); err != nil {
		t.Fatal(err)
	}
	mid, warn, err := svc.CreateActivity(ctx, oc, ActivityIn{Kind: "meeting",
		Title: "Gen Meeting", StartsAt: time.Now().Add(2 * time.Hour)})
	if err != nil || !warn {
		t.Fatalf("inside-notice meeting should warn, got warn=%v err=%v", warn, err)
	}

	// minutes: two drafts → versions 1,2; approve v2 archives nothing draft,
	// approve CAS on an approved row refuses
	v1, err := svc.DraftMinutes(ctx, oc, mid, "v1 body")
	if err != nil || v1 != 1 {
		t.Fatalf("minutes v1: %v %d", err, v1)
	}
	v2, err := svc.DraftMinutes(ctx, oc, mid, "v2 body")
	if err != nil || v2 != 2 {
		t.Fatalf("minutes v2: %v %d", err, v2)
	}
	var minutesID uuid.UUID
	pool.QueryRow(ctx, "SELECT id FROM minutes WHERE activity_id=$1 AND version=2", mid).Scan(&minutesID)
	if err := svc.ApproveMinutes(ctx, oc, mid, minutesID); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := svc.ApproveMinutes(ctx, oc, mid, minutesID); err != ErrInvalidTransition {
		t.Fatalf("re-approve should fail, got %v", err)
	}

	// action item → overdue sweep flips open→overdue
	itemID, err := svc.CreateActionItem(ctx, oc, mid, m1, "Book venue", time.Now().AddDate(0, 0, -2))
	if err != nil {
		t.Fatal(err)
	}
	if n, err := svc.OverdueActionItems(ctx); err != nil || n == 0 {
		t.Fatalf("overdue sweep: %v %d", err, n)
	}
	var st string
	pool.QueryRow(ctx, "SELECT state FROM action_items WHERE id=$1", itemID).Scan(&st)
	if st != "overdue" {
		t.Fatalf("expected overdue, got %s", st)
	}

	// attendance: bulk mark → late member fined via org.attendance.late_fine_minor
	if _, err := pool.Exec(ctx,
		`INSERT INTO org_settings (organization_id, key, value) VALUES ($1,'org.attendance.late_fine_minor','500')
		 ON CONFLICT (organization_id, key) DO UPDATE SET value='500'`, orgID); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkAttendance(ctx, oc, mid, []MarkIn{
		{MembershipID: m1, Status: "late"}, {MembershipID: m2, Status: "present"},
	}); err != nil {
		t.Fatal(err)
	}
	var fines int
	pool.QueryRow(ctx,
		`SELECT count(*) FROM obligations WHERE organization_id=$1 AND membership_id=$2
		   AND amount_minor=500`, orgID, m1).Scan(&fines)
	if fines != 1 {
		t.Fatalf("late fine should mint one obligation, got %d", fines)
	}

	// event capacity: cap=1 → second register is event_full; check-in flips
	eid, _, err := svc.CreateActivity(ctx, oc, ActivityIn{Kind: "event",
		Title: "Gala", StartsAt: time.Now().Add(72 * time.Hour), Capacity: intPtr(1)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterEvent(ctx, oc, eid, m1, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RegisterEvent(ctx, oc, eid, m2, "", ""); err != ErrEventFull {
		t.Fatalf("capacity should refuse, got %v", err)
	}
	if err := svc.CheckIn(ctx, oc, eid, m1, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	var rst string
	pool.QueryRow(ctx, "SELECT status FROM event_registrations WHERE activity_id=$1 AND membership_id=$2", eid, m1).Scan(&rst)
	if rst != "checked_in" {
		t.Fatalf("expected checked_in, got %s", rst)
	}

	// Rule 24: another org's IDs must not be readable/writable from oc
	oc2user := "act-o2-" + uuid.NewString()[:6] + "@t.co"
	var u2 uuid.UUID
	pool.QueryRow(ctx,
		`INSERT INTO users (email, display_name, password_hash, status) VALUES ($1,'u','x','active')
		 ON CONFLICT (email) DO UPDATE SET email=$1 RETURNING id`, oc2user).Scan(&u2)
	o2, err := orgSvc.CreateOrg(ctx, u2, "Other Org", "o2"+uuid.NewString()[:6], "union", "union", "NG", "Jos", "o@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc2, _ := orgSvc.ResolveOrgCtx(ctx, u2, o2.ID)
	if _, err := svc.Minutes(ctx, oc2, mid); err == nil {
		// org B asking for org A's minutes must see zero rows, not the list
	}
	list2, _ := svc.Minutes(ctx, oc2, mid)
	if len(list2) != 0 {
		t.Fatalf("cross-org minutes leak: %d rows", len(list2))
	}
	if err := svc.CheckIn(ctx, oc2, eid, m1, uuid.Nil); err == nil {
		t.Fatal("cross-org check-in should fail")
	}
	if err := svc.CompleteActionItem(ctx, oc2, itemID, false); err == nil {
		t.Fatal("cross-org manage-complete should fail")
	}

	// recurrence: FREQ=WEEKLY;COUNT=4 → 4 instances in horizon
	if _, err := pool.Exec(ctx,
		`INSERT INTO activity_recurrences (organization_id, kind, template, rrule) VALUES
		 ($1,'meeting', $2, 'FREQ=WEEKLY;COUNT=4')`, orgID,
		`{"title":"Weekly Standup","created_by":"`+oc.MembershipID.String()+`","duration_min":60}`); err != nil {
		t.Fatal(err)
	}
	if n, err := svc.GenerateRecurrences(ctx, 35); err != nil || n < 3 {
		t.Fatalf("recurrences: n=%d err=%v", n, err)
	}
	// generated instances link back via recurrence_id — the (recurrence_id,
	// starts_at) unique dedupes a replayed sweep atomically.
	var inst int
	pool.QueryRow(ctx, `SELECT count(*) FROM activities WHERE organization_id=$1 AND recurrence_id IS NOT NULL`, orgID).Scan(&inst)
	if inst < 3 {
		t.Fatalf("expected ≥3 generated instances, got %d", inst)
	}
	// replaying the sweep must not mint duplicates (the rec:-tag dedupe
	// this replaced created a fresh set every daily run).
	if n, err := svc.GenerateRecurrences(ctx, 35); err != nil || n != 0 {
		t.Fatalf("replayed sweep should mint 0, got n=%d err=%v", n, err)
	}
}

func intPtr(i int) *int { return &i }
