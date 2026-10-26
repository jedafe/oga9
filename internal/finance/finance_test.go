package finance

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/tenant"
)

func testSvc(t *testing.T) (*Service, *tenant.Service, *pgxpool.Pool) {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), "postgres://oga9:oga9@localhost:5434/oga9_test?sslmode=disable")
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
	return NewService(pool, vk), tenant.NewService(pool, vk), pool
}

func mkUser(t *testing.T, pool *pgxpool.Pool, email string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := pool.Exec(context.Background(),
		"INSERT INTO users (id, email, password_hash, display_name, status) VALUES ($1,$2,'x','T','active')", id, email)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// activeMember invites a user in and forces lifecycle to active (the
// intake path is exercised in the member suite).
func activeMember(t *testing.T, svc *tenant.Service, pool *pgxpool.Pool, oc *tenant.OrgCtx, email string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	u := mkUser(t, pool, email)
	inv, err := svc.CreateInvitation(ctx, oc, email, "", "", "floor")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AcceptInvitation(ctx, u, inv.Token); err != nil {
		t.Fatal(err)
	}
	moc, err := svc.ResolveOrgCtx(ctx, u, oc.OrgID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx,
		"UPDATE organization_memberships SET lifecycle_state='active' WHERE id=$1", moc.MembershipID); err != nil {
		t.Fatal(err)
	}
	return u
}

// §12 scenarios: obligation generation + idempotent replay, record→approve
// SoD, partial→paid, reversal, claim→confirm provenance, drive disburse.
func TestFinanceFlow(t *testing.T) {
	svc, orgSvc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "fo"+uuid.NewString()[:8]+"@test.oga9")

	org, err := orgSvc.CreateOrg(ctx, owner, "Fin Union", "fu"+uuid.NewString()[:6], "union", "union", "NG", "Kano", "f@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := orgSvc.ResolveOrgCtx(ctx, owner, org.ID)
	payer := activeMember(t, orgSvc, pool, oc, "fp"+uuid.NewString()[:8]+"@t.co")
	payerOC, _ := orgSvc.ResolveOrgCtx(ctx, payer, org.ID)

	ctID, err := svc.CreateContributionType(ctx, oc, "dues", "Monthly dues", "assessed")
	if err != nil {
		t.Fatal(err)
	}
	schedID, err := svc.CreateSchedule(ctx, oc, ScheduleIn{
		ContributionTypeID: ctID, Name: "Dues 2026", AmountMinor: 500000,
		Currency: "NGN", Recurrence: "monthly", AppliesTo: json.RawMessage(`{}`),
		StartsOn: time.Now().Add(-24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}

	// both active floor members get an obligation; replay mints zero
	n, err := svc.GenerateObligations(ctx, schedID)
	if err != nil || n != 2 {
		t.Fatalf("generate: n=%d err=%v", n, err)
	}
	if n2, _ := svc.GenerateObligations(ctx, schedID); n2 != 0 {
		t.Fatalf("generation replay minted %d obligations", n2)
	}
	obs, _ := svc.ListObligations(ctx, oc, payerOC.MembershipID, "", "")
	if len(obs) != 1 {
		t.Fatalf("payer obligations: %d", len(obs))
	}
	ob := obs[0]

	// idempotency: same key replays the original
	pid, replay, err := svc.RecordPayment(ctx, payerOC, PaymentIn{
		ObligationID: uuid.UUID(ob.ID.Bytes), AmountMinor: 250000, Currency: "NGN",
		Channel: "cash", IdempotencyKey: "k-" + uuid.NewString()[:8]})
	if err != nil || replay {
		t.Fatalf("record: %v replay=%v", err, replay)
	}
	// recorder can't approve (separation of duties, enforced in the CAS)
	if err := svc.ConfirmPayment(ctx, payerOC, pid); err != ErrSeparation {
		t.Fatalf("self-approve should fail, got %v", err)
	}
	// different member approves → confirmed + ledger + receipt + partial
	if err := svc.ConfirmPayment(ctx, oc, pid); err != nil {
		t.Fatalf("approve: %v", err)
	}
	cur, _ := svc.ListObligations(ctx, oc, payerOC.MembershipID, "", "")
	if cur[0].Status != "partially_paid" {
		t.Fatalf("expected partially_paid, got %s", cur[0].Status)
	}
	var rno string
	if err := pool.QueryRow(ctx, "SELECT receipt_no FROM receipts WHERE payment_id=$1", pid).Scan(&rno); err != nil || rno == "" {
		t.Fatalf("receipt: %q %v", rno, err)
	}

	// full second payment → paid
	pid2, _, _ := svc.RecordPayment(ctx, payerOC, PaymentIn{
		ObligationID: uuid.UUID(ob.ID.Bytes), AmountMinor: 250000, Currency: "NGN",
		Channel: "transfer", IdempotencyKey: "k2-" + uuid.NewString()[:8]})
	if err := svc.ConfirmPayment(ctx, oc, pid2); err != nil {
		t.Fatal(err)
	}
	cur, _ = svc.ListObligations(ctx, oc, payerOC.MembershipID, "", "")
	if cur[0].Status != "paid" {
		t.Fatalf("expected paid, got %s", cur[0].Status)
	}

	// reversal: append-only ledger gets a debit, original untouched
	if err := svc.ReversePayment(ctx, oc, pid, "wrong amount"); err != nil {
		t.Fatal(err)
	}
	var debits int64
	pool.QueryRow(ctx, "SELECT count(*) FROM ledger_entries WHERE payment_id=$1 AND direction='debit'", pid).Scan(&debits)
	if debits != 1 {
		t.Fatalf("expected 1 reversing entry, got %d", debits)
	}

	// claim provenance: member submits, officer confirms → same trail
	cid, err := svc.SubmitClaim(ctx, payerOC, PaymentIn{
		AmountMinor: 100000, Currency: "NGN", Channel: "cash"}, "dues top-up", uuid.Nil)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := svc.ConfirmClaim(ctx, oc, cid, "verified", uuid.Nil, uuid.Nil); err != nil {
		t.Fatalf("confirm claim: %v", err)
	}
	var cpCount int64
	pool.QueryRow(ctx, "SELECT count(*) FROM payments WHERE claim_id=$1 AND status='confirmed'", cid).Scan(&cpCount)
	if cpCount != 1 {
		t.Fatalf("confirmed claim should produce a payment, got %d", cpCount)
	}

	// drive: open → claim-paid → close → disburse posts a debit
	did, err := svc.OpenDrive(ctx, oc, DriveIn{Title: "Wedding", Kind: "fixed_amount", FixedAmountMinor: 1000000})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.OpenDrive(ctx, oc, DriveIn{Title: "Bad", Kind: "fixed_amount"}); err != ErrDriveKind {
		t.Fatalf("fixed_amount without amount should fail, got %v", err)
	}
	cid2, _ := svc.SubmitClaim(ctx, payerOC, PaymentIn{
		DriveID: did, AmountMinor: 1000000, Currency: "NGN", Channel: "cash"}, "", uuid.Nil)
	if err := svc.ConfirmClaim(ctx, oc, cid2, "", uuid.Nil, uuid.Nil); err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseDrive(ctx, oc, did); err != nil {
		t.Fatal(err)
	}
	if err := svc.DisburseDrive(ctx, oc, did, "handed to family"); err != nil {
		t.Fatal(err)
	}
	var disbursed string
	pool.QueryRow(ctx, "SELECT state FROM contribution_drives WHERE id=$1", did).Scan(&disbursed)
	if disbursed != "disbursed" {
		t.Fatalf("drive state: %s", disbursed)
	}

	// cross-tenant: another org's finance reads return empty, never leak
	org2, err := orgSvc.CreateOrg(ctx, owner, "Other", "ot"+uuid.NewString()[:6], "union", "union", "NG", "Enugu", "o2@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc2, err := orgSvc.ResolveOrgCtx(ctx, owner, org2.ID)
	if err != nil {
		t.Fatal(err)
	}
	obs2, _ := svc.ListObligations(ctx, oc2, uuid.Nil, "", "")
	if len(obs2) != 0 {
		t.Fatal("cross-org obligation leak")
	}
}

// Review guards: claim dedupe 409 (not 500), rejected-claim resubmit,
// idempotency-key payload conflict, disbursed-drive confirm refusal,
// ends_on:null clear, one-payment-one-renewal.
func TestFinanceGuards(t *testing.T) {
	svc, orgSvc, pool := testSvc(t)
	ctx := context.Background()
	owner := mkUser(t, pool, "go"+uuid.NewString()[:8]+"@test.oga9")
	org, err := orgSvc.CreateOrg(ctx, owner, "Guard Union", "gu"+uuid.NewString()[:6], "union", "union", "NG", "Jos", "g@t.co", uuid.Nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	oc, _ := orgSvc.ResolveOrgCtx(ctx, owner, org.ID)
	payer := activeMember(t, orgSvc, pool, oc, "gp"+uuid.NewString()[:8]+"@t.co")
	payerOC, _ := orgSvc.ResolveOrgCtx(ctx, payer, org.ID)

	// claim dedupe: same ref twice → ErrAlreadyClaimed (was a 500 via ErrNoRows)
	// receipt_media_id FKs into documents (g11) — a real row is required.
	var docID uuid.UUID
	pool.QueryRow(ctx,
		`INSERT INTO documents (organization_id, kind, title, storage_key, uploaded_by)
		 VALUES ($1,'photo','proof','k',$2) RETURNING id`, org.ID, payerOC.MembershipID).Scan(&docID)
	in := PaymentIn{AmountMinor: 1000, Currency: "NGN", Channel: "transfer", ExternalRef: "REF-" + uuid.NewString()[:8]}
	cid, err := svc.SubmitClaim(ctx, payerOC, in, "", docID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitClaim(ctx, payerOC, in, "", docID); err != ErrAlreadyClaimed {
		t.Fatalf("dup claim should be ErrAlreadyClaimed, got %v", err)
	}
	// reject → same ref resubmits fine (rejection doesn't burn the ref)
	if err := svc.RejectClaim(ctx, oc, cid, "no credit seen"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SubmitClaim(ctx, payerOC, in, "", docID); err != nil {
		t.Fatalf("resubmit after reject should pass, got %v", err)
	}

	// idempotency key bound to a different payload → conflict, not replay
	key := "k-" + uuid.NewString()[:8]
	if _, _, err := svc.RecordPayment(ctx, payerOC, PaymentIn{
		AmountMinor: 5000, Currency: "NGN", Channel: "cash", IdempotencyKey: key}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RecordPayment(ctx, payerOC, PaymentIn{
		AmountMinor: 9999, Currency: "NGN", Channel: "cash", IdempotencyKey: key}); err != ErrIdempotencyConflict {
		t.Fatalf("key+payload mismatch should conflict, got %v", err)
	}

	// disbursed drive refuses late confirms
	did, err := svc.OpenDrive(ctx, oc, DriveIn{Title: "D"})
	if err != nil {
		t.Fatal(err)
	}
	pid, _, err := svc.RecordPayment(ctx, payerOC, PaymentIn{
		DriveID: did, AmountMinor: 100, Currency: "NGN", Channel: "cash",
		IdempotencyKey: "k3-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.CloseDrive(ctx, oc, did); err != nil {
		t.Fatal(err)
	}
	if err := svc.DisburseDrive(ctx, oc, did, "out"); err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmPayment(ctx, oc, pid); err != ErrInvalidTransition {
		t.Fatalf("confirm on disbursed drive should refuse, got %v", err)
	}

	// ends_on clear: set then clear via clearEndsOn
	ctID, _ := svc.CreateContributionType(ctx, oc, "levy", "Levy", "assessed")
	sid, err := svc.CreateSchedule(ctx, oc, ScheduleIn{
		ContributionTypeID: ctID, Name: "S", AmountMinor: 100, Currency: "NGN",
		Recurrence: "once", StartsOn: time.Now(), EndsOn: time.Now().AddDate(0, 1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UpdateSchedule(ctx, oc, sid, ScheduleIn{}, nil, true); err != nil {
		t.Fatal(err)
	}
	var hasEnd bool
	pool.QueryRow(ctx, "SELECT ends_on IS NOT NULL FROM dues_schedules WHERE id=$1", sid).Scan(&hasEnd)
	if hasEnd {
		t.Fatal("ends_on:null should clear the end date")
	}

	// one payment settles one renewal only
	rp, _, err := svc.RecordPayment(ctx, payerOC, PaymentIn{
		AmountMinor: 2000, Currency: "NGN", Channel: "cash", IdempotencyKey: "k4-" + uuid.NewString()[:8]})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.ConfirmPayment(ctx, oc, rp); err != nil {
		t.Fatal(err)
	}
	if _, err := orgSvc.RenewMember(ctx, oc, payerOC.MembershipID, "2026", time.Now(), time.Now().AddDate(1, 0, 0), rp); err != nil {
		t.Fatal(err)
	}
	if _, err := orgSvc.RenewMember(ctx, oc, payerOC.MembershipID, "2027", time.Now(), time.Now().AddDate(2, 0, 0), rp); err != tenant.ErrBadValue {
		t.Fatalf("same payment on second renewal should fail, got %v", err)
	}
}
