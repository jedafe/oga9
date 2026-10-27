// Ref: oga9-engineering/sections/21-elections.md — secrecy model,
// eligibility, double-vote guard, ratification seating.
package elections

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
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

type fixture struct {
	oc       *tenant.OrgCtx
	posID    uuid.UUID
	termID   uuid.UUID
	freshMid uuid.UUID // recently joined — fails min_tenure
	voterCtx *tenant.OrgCtx
}

func mk(t *testing.T, pool *pgxpool.Pool) fixture {
	t.Helper()
	ctx := context.Background()
	suffix := uuid.NewString()[:8]
	var tid, oid, uid, mid, uid2, mid2, termID, posID pgtype.UUID
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
	for _, u := range []*pgtype.UUID{&uid, &uid2} {
		if err := pool.QueryRow(ctx,
			`INSERT INTO users (email, password_hash, display_name, status) VALUES ($1,'x',$2,'active') RETURNING id`,
			uuid.NewString()[:8]+"@x.test", "U "+suffix).Scan(u); err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, category, lifecycle_state, joined_at)
		 VALUES ($1,$2,'regular','active',now()-interval '500 days') RETURNING id`, oid, uid).Scan(&mid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id, user_id, category, lifecycle_state, joined_at)
		 VALUES ($1,$2,'regular','active',now()-interval '500 days') RETURNING id`, oid, uid2).Scan(&mid2); err != nil {
		t.Fatal(err)
	}
	// next term (elections elect INTO it)
	if err := pool.QueryRow(ctx,
		`INSERT INTO terms (organization_id, label, starts_at, ends_at, is_current)
		 VALUES ($1,'2028-2030',now()::date+interval '2 years',now()::date+interval '4 years',false) RETURNING id`, oid).Scan(&termID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO org_positions (organization_id, name, slug, is_executive, seats)
		 VALUES ($1,'President','president',true,1) RETURNING id`, oid).Scan(&posID); err != nil {
		t.Fatal(err)
	}
	var freshMid uuid.UUID
	var fuid pgtype.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (email,password_hash,display_name,status) VALUES ($1,'x','f','active') RETURNING id`,
		"f"+suffix+"@x.test").Scan(&fuid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO organization_memberships (organization_id,user_id,category,lifecycle_state,joined_at)
		 VALUES ($1,$2,'regular','active',now()-interval '2 days') RETURNING id`, oid, fuid).Scan(&freshMid); err != nil {
		t.Fatal(err)
	}
	return fixture{
		oc: &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
			UserID: uuid.UUID(uid.Bytes), MembershipID: uuid.UUID(mid.Bytes), State: "active"},
		voterCtx: &tenant.OrgCtx{OrgID: uuid.UUID(oid.Bytes), TenantID: uuid.UUID(tid.Bytes),
			UserID: uuid.UUID(uid2.Bytes), MembershipID: uuid.UUID(mid2.Bytes), State: "active"},
		posID: uuid.UUID(posID.Bytes), termID: uuid.UUID(termID.Bytes), freshMid: freshMid,
	}
}

var electionRules = json.RawMessage(`{"min_tenure_days":180,"categories":["regular"],"requires_good_standing":true}`)

// TestSecrecyAndGuards — spec tests 1+2: ineligible members can't be
// nominated or vote; a second vote is already_voted; votes carry no
// correlatable field.
func TestSecrecyAndGuards(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	f := mk(t, pool)

	eid, err := svc.Create(ctx, f.oc, f.termID, "2028 General", electionRules)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.BeginNominations(ctx, f.oc, eid); err != nil {
		t.Fatal(err)
	}
	// fresh member fails min_tenure at nominate (test 1)
	if _, err := svc.Nominate(ctx, f.oc, eid, f.posID, f.freshMid, uuid.Nil, ""); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("fresh member nominate: %v", err)
	}
	cid, err := svc.Nominate(ctx, f.oc, eid, f.posID, f.oc.MembershipID, uuid.Nil, "manifesto")
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Vet(ctx, f.oc, eid, cid, "vetted", ""); err != nil {
		t.Fatal(err)
	}
	// duplicate nomination → already_nominated
	if _, err := svc.Nominate(ctx, f.oc, eid, f.posID, f.oc.MembershipID, uuid.Nil, ""); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("dup nominate: %v", err)
	}
	if err := svc.OpenVoting(ctx, f.oc, eid); err != nil {
		t.Fatal(err)
	}
	// ineligible voter refused (test 1 — fresh member)
	fresh := &tenant.OrgCtx{OrgID: f.oc.OrgID, TenantID: f.oc.TenantID, MembershipID: f.freshMid, State: "active"}
	if err := svc.Vote(ctx, fresh, eid, []VoteChoice{{PositionID: f.posID, CandidateID: cid}}); !errors.Is(err, ErrNotEligible) {
		t.Fatalf("ineligible vote: %v", err)
	}
	// eligible voter casts; a second attempt → already_voted (test 2)
	if err := svc.Vote(ctx, f.voterCtx, eid, []VoteChoice{{PositionID: f.posID, CandidateID: cid}}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Vote(ctx, f.voterCtx, eid, []VoteChoice{{PositionID: f.posID, CandidateID: cid}}); !errors.Is(err, ErrAlreadyVoted) {
		t.Fatalf("second vote: %v", err)
	}
	// secrecy: no vote row carries a member/ballot link
	var leak bool
	pool.QueryRow(ctx, `SELECT EXISTS(
		SELECT 1 FROM information_schema.columns
		WHERE table_name='votes' AND column_name IN ('membership_id','ballot_id','user_id','member_id'))`).Scan(&leak)
	if leak {
		t.Fatal("votes table carries a correlatable column — secrecy broken")
	}
}

// TestRatifySeats — spec test 3: close → tally; ratify → appointments
// land for the elected (next) term.
func TestRatifySeats(t *testing.T) {
	svc, pool := testSvc(t)
	ctx := context.Background()
	f := mk(t, pool)

	// wire the appointment seam to a direct insert (governance's real
	// writer is exercised by its own tests; here we assert the seam is
	// called with term+position+member).
	var seated []uuid.UUID
	svc.SetAppoint(func(_ context.Context, _ *tenant.OrgCtx, termID, posID, memberID, branchID, committeeID uuid.UUID, _ time.Time) (uuid.UUID, error) {
		if termID != f.termID {
			t.Errorf("seat bound to wrong term: %s", termID)
		}
		seated = append(seated, memberID)
		return uuid.New(), nil
	})

	eid, _ := svc.Create(ctx, f.oc, f.termID, "E", json.RawMessage(`{}`))
	_ = svc.BeginNominations(ctx, f.oc, eid)
	cid, _ := svc.Nominate(ctx, f.oc, eid, f.posID, f.oc.MembershipID, uuid.Nil, "")
	_ = svc.Vet(ctx, f.oc, eid, cid, "vetted", "")
	_ = svc.OpenVoting(ctx, f.oc, eid)
	_ = svc.Vote(ctx, f.voterCtx, eid, []VoteChoice{{PositionID: f.posID, CandidateID: cid}})
	if err := svc.CloseVoting(ctx, f.oc, eid); err != nil {
		t.Fatal(err)
	}
	if err := svc.Ratify(ctx, f.oc, eid); err != nil {
		t.Fatal(err)
	}
	if len(seated) != 1 || seated[0] != f.oc.MembershipID {
		t.Fatalf("expected winner seated, got %v", seated)
	}
	e, _ := svc.Get(ctx, f.oc, eid)
	if e.State != "ratified" {
		t.Fatalf("state: %s", e.State)
	}
	// audit trail covers the transitions + the cast (test 4)
	trail, err := svc.AuditTrail(ctx, f.oc, eid)
	if err != nil || len(trail) < 4 {
		t.Fatalf("audit trail thin: %d %v", len(trail), err)
	}
}
