// Ref: oga9-engineering/sections/12-finance-lite.md — dues & finance-lite.
// Record-keeping-first (ADR-012 amendment): no gateway — every credit is
// verified, by claim→confirm (member path) or record→approve (officer
// path, recorder != approver). Ledger is append-only (Rule 19);
// corrections are reversing entries. Payment creation is idempotent via
// Idempotency-Key (Rule 9).
package finance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound            = errors.New("finance: not_found")
	ErrInvalidTransition   = errors.New("finance: invalid_state_transition")
	ErrSeparation          = errors.New("finance: recorder_approver_same")
	ErrDriveKind           = errors.New("finance: drive_fixed_amount_required")
	ErrAlreadyClaimed      = errors.New("finance: claim_duplicate")
	ErrIdempotencyConflict = errors.New("finance: idempotency_key_in_use")
	ErrBadChannel          = errors.New("finance: invalid_channel")
	ErrBadValue            = errors.New("finance: invalid_value")
)

// paymentChannels — §12 MVP manual channels on payments rows
// (cash|transfer|other_manual); claims additionally allow 'pos' per the
// payment_claims schema comment.
var paymentChannels = map[string]bool{
	"cash": true, "transfer": true, "other_manual": true,
}
var claimChannels = map[string]bool{
	"cash": true, "transfer": true, "pos": true, "other_manual": true,
}

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	vk   valkey.Client
	// activityGate validates an event/meeting id belongs to the org —
	// wired from activities.Service (Rule 26: finance never reads the
	// activities tables itself).
	activityGate func(ctx context.Context, orgID, activityID uuid.UUID) bool
}

func NewService(pool *pgxpool.Pool, vk valkey.Client) *Service {
	return &Service{pool: pool, q: db.New(pool), vk: vk}
}

// SetActivityGate wires the activities-module check for event-linked
// payments (payments.activity_id).
func (s *Service) SetActivityGate(fn func(ctx context.Context, orgID, activityID uuid.UUID) bool) {
	s.activityGate = fn
}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }
func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: !t.IsZero()} }

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ---------- contribution types + schedules ----------

func (s *Service) CreateContributionType(ctx context.Context, oc *tenant.OrgCtx, key, label, kind string) (uuid.UUID, error) {
	if kind == "" {
		kind = "assessed"
	}
	row, err := s.q.CreateContributionType(ctx, db.CreateContributionTypeParams{
		OrganizationID: toPg(oc.OrgID), Key: key, Label: label, Kind: kind})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(row.ID), nil
}

func (s *Service) ListContributionTypes(ctx context.Context, oc *tenant.OrgCtx) ([]db.ContributionTypes, error) {
	return s.q.ListContributionTypes(ctx, toPg(oc.OrgID))
}

type ScheduleIn struct {
	ContributionTypeID uuid.UUID       `json:"contribution_type_id"`
	Name               string          `json:"name"`
	AmountMinor        int64           `json:"amount_minor"`
	Currency           string          `json:"currency"`
	Recurrence         string          `json:"recurrence"` // monthly|quarterly|annual|once
	AppliesTo          json.RawMessage `json:"applies_to"`
	StartsOn           time.Time       `json:"starts_on"`
	EndsOn             time.Time       `json:"ends_on"`
}

func (s *Service) CreateSchedule(ctx context.Context, oc *tenant.OrgCtx, in ScheduleIn) (uuid.UUID, error) {
	if in.AmountMinor <= 0 || in.Currency == "" || in.Recurrence == "" {
		return uuid.Nil, errors.New("amount, currency, recurrence required")
	}
	if len(in.AppliesTo) == 0 {
		in.AppliesTo = json.RawMessage("{}") // empty filter = all active members
	}
	ct, err := s.q.GetContributionType(ctx, db.GetContributionTypeParams{
		ID: toPg(in.ContributionTypeID), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	var row db.DuesSchedules
	err = s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateDuesSchedule(ctx, db.CreateDuesScheduleParams{
			OrganizationID: toPg(oc.OrgID), ContributionTypeID: ct.ID,
			Name: in.Name, AmountMinor: in.AmountMinor, Currency: in.Currency,
			Recurrence: in.Recurrence, AppliesTo: in.AppliesTo,
			StartsOn: pgDate(in.StartsOn), EndsOn: pgDate(in.EndsOn), Active: true})
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DuesSchedulePublished",
			"dues_schedule", fromPg(row.ID), oc.UserID, true,
			map[string]any{"schedule_id": fromPg(row.ID)})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(row.ID), nil
}

func (s *Service) ListSchedules(ctx context.Context, oc *tenant.OrgCtx) ([]db.DuesSchedules, error) {
	return s.q.ListDuesSchedules(ctx, toPg(oc.OrgID))
}

// (DeactivateSchedule removed — UpdateSchedule with {"active":false} is the
// catalog PATCH surface.)

// ---------- obligation generation (finance:generate_obligations, Rule 23) ----------

// PeriodLabel derives the current period tag from the schedule recurrence.
func PeriodLabel(recurrence string, at time.Time) string {
	switch recurrence {
	case "monthly":
		return at.Format("2006-01")
	case "quarterly":
		return fmt.Sprintf("%d-Q%d", at.Year(), (int(at.Month())-1)/3+1)
	case "annual":
		return at.Format("2006")
	default:
		return "once"
	}
}

type appliesTo struct {
	Categories         []string    `json:"categories"`
	UnitIDs            []uuid.UUID `json:"unit_ids"`
	IncludeLifeMembers bool        `json:"include_life_members"` // §11: life members are dues-exempt per org rule — opt in here
}

// GenerateObligations fans a schedule out to every matching active member.
// Idempotent: the (membership,type,period) unique makes replays no-ops —
// only freshly inserted rows emit ObligationCreated.
func (s *Service) GenerateObligations(ctx context.Context, scheduleID uuid.UUID) (int, error) {
	var scheds []db.DuesSchedules
	var err error
	if scheduleID == uuid.Nil {
		scheds, err = s.q.ActiveDuesSchedules(ctx)
	} else {
		var sc db.DuesSchedules
		if sc, err = s.q.GetDuesScheduleByID(ctx, toPg(scheduleID)); err != nil {
			return 0, ErrNotFound
		}
		scheds = []db.DuesSchedules{sc}
	}
	if err != nil {
		return 0, err
	}
	now := time.Now()
	created := 0
	for _, sc := range scheds {
		// 'registration' schedules mint per-member on MemberApproved via
		// the consumer — periodic generation must never fan them out to
		// the existing roster (double-charge; the sweep's query already
		// excludes them, the explicit-id path needs this guard).
		if ct, err := s.q.GetContributionType(ctx, db.GetContributionTypeParams{
			ID: sc.ContributionTypeID, OrganizationID: sc.OrganizationID}); err == nil &&
			ct.Key == "registration" {
			continue
		}
		var f appliesTo
		_ = json.Unmarshal(sc.AppliesTo, &f)
		unitIDs := make([]pgtype.UUID, len(f.UnitIDs))
		for i, u := range f.UnitIDs {
			unitIDs[i] = toPg(u)
		}
		members, err := s.q.ObligationPayees(ctx, db.ObligationPayeesParams{
			OrganizationID: sc.OrganizationID,
			Categories:     f.Categories,
			UnitIds:        unitIDs,
			IncludeLife:    f.IncludeLifeMembers})
		if err != nil {
			return created, err
		}
		period := PeriodLabel(sc.Recurrence, now)
		if sc.Recurrence == "once" {
			// 'once' shares its label across every one-off schedule of the
			// same type — scope it so a second one-off still generates.
			period = "once:" + fromPg(sc.ID).String()[:8]
		}
		// due_date: end of the covered period (month/quarter/year end);
		// 'once' schedules use their own start date.
		due := sc.StartsOn.Time
		switch sc.Recurrence {
		case "monthly":
			due = time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, time.UTC)
		case "quarterly":
			qEnd := time.Month((int(now.Month())-1)/3*3 + 3)
			due = time.Date(now.Year(), qEnd+1, 0, 0, 0, 0, 0, time.UTC)
		case "annual":
			due = time.Date(now.Year()+1, 1, 0, 0, 0, 0, 0, time.UTC)
		}
		// domain events carry the org's tenant — sweeps/tasks have no OrgCtx
		var tenantID uuid.UUID
		if t, err := s.q.GetOrgTenantID(ctx, sc.OrganizationID); err == nil {
			tenantID = fromPg(t)
		}
		err = s.withTx(ctx, func(q *db.Queries) error {
			for _, mid := range members {
				row, err := q.CreateObligation(ctx, db.CreateObligationParams{
					OrganizationID: sc.OrganizationID, MembershipID: mid,
					ContributionTypeID: sc.ContributionTypeID, ScheduleID: sc.ID,
					PeriodLabel: pgText(period), AmountMinor: sc.AmountMinor,
					Currency: sc.Currency, DueDate: pgDate(due)})
				if err == pgx.ErrNoRows {
					continue // period obligation already exists — idempotent replay
				}
				if err != nil {
					return err
				}
				created++
				if err := events.Emit(ctx, q, tenantID, fromPg(sc.OrganizationID),
					"ObligationCreated", "obligation", fromPg(row.ID), uuid.Nil, true,
					map[string]any{"obligation_id": fromPg(row.ID),
						"member_id": fromPg(row.MembershipID), "amount_minor": row.AmountMinor}); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return created, err
		}
	}
	return created, nil
}

func (s *Service) ListObligations(ctx context.Context, oc *tenant.OrgCtx, memberID uuid.UUID, status, period string) ([]db.Obligations, error) {
	return s.q.ListObligations(ctx, db.ListObligationsParams{
		OrganizationID: toPg(oc.OrgID), Column2: toPg(memberID), Column3: status, Column4: period})
}

func (s *Service) WaiveObligation(ctx context.Context, oc *tenant.OrgCtx, oid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.WaiveObligation(ctx, db.WaiveObligationParams{
			ID: toPg(oid), OrganizationID: toPg(oc.OrgID), WaivedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrInvalidTransition
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ObligationWaived",
			"obligation", oid, oc.UserID, true,
			map[string]any{"obligation_id": oid, "by": oc.MembershipID})
	})
}

// refreshObligation recomputes an obligation's status from confirmed money.
// Runs inside the calling tx so status stays atomic with the payment.
func refreshObligation(ctx context.Context, q *db.Queries, orgID, oid pgtype.UUID) error {
	if !oid.Valid {
		return nil
	}
	ob, err := q.GetObligation(ctx, db.GetObligationParams{ID: oid, OrganizationID: orgID})
	// waived/cancelled are terminal; 'paid' MUST recompute — a reversal
	// legitimately un-pays the obligation (SetObligationStatus allows it).
	if err != nil || ob.Status == "waived" || ob.Status == "cancelled" {
		return err
	}
	paid, err := q.ConfirmedPaymentsForObligation(ctx, oid)
	if err != nil {
		return err
	}
	status := "open"
	if paid >= ob.AmountMinor {
		status = "paid"
	} else if paid > 0 {
		status = "partially_paid"
	}
	// 'overdue' is sweep-managed: a reversal can't un-overdue — only full
	// settlement (or a waiver) clears it; the next sweep re-marks anyway.
	if status != "paid" && ob.Status == "overdue" {
		status = "overdue"
	}
	_, err = q.SetObligationStatus(ctx, db.SetObligationStatusParams{
		ID: ob.ID, OrganizationID: ob.OrganizationID, Status: status})
	return err
}

// ---------- payments ----------

type PaymentIn struct {
	MembershipID    uuid.UUID `json:"membership_id"`
	ObligationID    uuid.UUID `json:"obligation_id"`
	DriveID         uuid.UUID `json:"drive_id"`
	ActivityID      uuid.UUID `json:"activity_id"` // event-linked contribution (§12 donations view)
	BenefitMemberID uuid.UUID `json:"benefit_member_id"`
	AmountMinor     int64     `json:"amount_minor"`
	Currency        string    `json:"currency"`
	Channel         string    `json:"channel"` // cash|transfer|pos|other_manual
	ExternalRef     string    `json:"external_ref"`
	HideContributor bool      `json:"hide_contributor"` // §12: member hides per-claim drive display
	IdempotencyKey  string    `json:"-"`
}

// RecordPayment creates an 'initiated' payment — pending until a different
// member approves it. Rule 9: a replayed Idempotency-Key returns the
// original row instead of double-recording.
func (s *Service) RecordPayment(ctx context.Context, oc *tenant.OrgCtx, in PaymentIn) (uuid.UUID, bool, error) {
	if in.AmountMinor <= 0 || in.Currency == "" {
		return uuid.Nil, false, ErrBadValue
	}
	if !paymentChannels[in.Channel] {
		return uuid.Nil, false, ErrBadChannel
	}
	if in.DriveID != uuid.Nil {
		d, err := s.q.GetDrive(ctx, db.GetDriveParams{ID: toPg(in.DriveID), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return uuid.Nil, false, ErrNotFound
		}
		if d.State != "open" {
			return uuid.Nil, false, ErrInvalidTransition // closed/disbursed drives take no money
		}
		if d.Kind == "fixed_amount" && d.FixedAmountMinor.Valid && in.AmountMinor != d.FixedAmountMinor.Int64 {
			return uuid.Nil, false, ErrDriveKind // fixed drives take exactly their amount
		}
	}
	payer := in.MembershipID
	if payer == uuid.Nil {
		payer = oc.MembershipID
	}
	// payee + beneficiary memberships must be THIS org's — the FK only
	// proves existence, so a foreign membership id would pass otherwise.
	for _, mid := range []uuid.UUID{payer, in.BenefitMemberID} {
		if mid == uuid.Nil {
			continue
		}
		if org, err := s.q.GetMembershipOrg(ctx, toPg(mid)); err != nil || fromPg(org) != oc.OrgID {
			return uuid.Nil, false, ErrNotFound
		}
	}
	// Cross-org link check + ownership: an obligation/drive FK from another
	// org would corrupt that org's roll (the FK only checks existence), and
	// a payment tagged to an obligation must come from that obligation's
	// member — crediting member A's debt from member B's remittance breaks
	// the "who's paid" roll call (pay on behalf via ad-hoc, not obligation).
	if in.ObligationID != uuid.Nil {
		ob, err := s.q.GetObligation(ctx, db.GetObligationParams{
			ID: toPg(in.ObligationID), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return uuid.Nil, false, ErrNotFound
		}
		if fromPg(ob.MembershipID) != payer {
			return uuid.Nil, false, ErrBadValue
		}
	}
	if in.ActivityID != uuid.Nil {
		// event-linked contribution — the activity must exist in THIS org
		// (validated through the activities seam, Rule 26).
		if s.activityGate == nil || !s.activityGate(ctx, oc.OrgID, in.ActivityID) {
			return uuid.Nil, false, ErrNotFound
		}
	}
	if in.IdempotencyKey != "" {
		if p, err := s.q.PaymentByIdempotencyKey(ctx, db.PaymentByIdempotencyKeyParams{
			OrganizationID: toPg(oc.OrgID), IdempotencyKey: pgText(in.IdempotencyKey)}); err == nil {
			if !samePaymentIntent(p, in, payer) {
				return uuid.Nil, false, ErrIdempotencyConflict // key reuse with a different payment
			}
			return fromPg(p.ID), true, nil // replay — original stands
		}
	}
	var row db.Payments
	var raced bool
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreatePayment(ctx, db.CreatePaymentParams{
			OrganizationID: toPg(oc.OrgID), MembershipID: toPg(payer),
			ObligationID: toPg(in.ObligationID), AmountMinor: in.AmountMinor,
			Currency: in.Currency, Channel: in.Channel, ExternalRef: pgText(in.ExternalRef),
			BenefitMemberID: toPg(in.BenefitMemberID), DriveID: toPg(in.DriveID),
			ActivityID:     toPg(in.ActivityID),
			IdempotencyKey: pgText(in.IdempotencyKey), RecordedBy: toPg(oc.MembershipID)})
		if err == pgx.ErrNoRows {
			raced = true // key raced in concurrently — return the winner below
			return nil
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "PaymentInitiated",
			"payment", fromPg(row.ID), oc.UserID, true,
			map[string]any{"payment_id": fromPg(row.ID), "channel": row.Channel})
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	if raced {
		p, err := s.q.PaymentByIdempotencyKey(ctx, db.PaymentByIdempotencyKeyParams{
			OrganizationID: toPg(oc.OrgID), IdempotencyKey: pgText(in.IdempotencyKey)})
		if err != nil {
			// Key collided with another org's payment (global unique) —
			// surface a conflict, not a 500.
			if org, e2 := s.q.PaymentOrgForIdempotencyKey(ctx, pgText(in.IdempotencyKey)); e2 == nil &&
				fromPg(org) != oc.OrgID {
				return uuid.Nil, false, ErrIdempotencyConflict
			}
			return uuid.Nil, false, err
		}
		if !samePaymentIntent(p, in, payer) {
			return uuid.Nil, false, ErrIdempotencyConflict
		}
		return fromPg(p.ID), true, nil
	}
	return fromPg(row.ID), false, nil
}

// samePaymentIntent compares a replayed request against the stored payment
// — an Idempotency-Key must bind to ONE logical payment (Rule 9); key
// reuse with a different payload is a conflict, not a replay.
func samePaymentIntent(p db.Payments, in PaymentIn, payer uuid.UUID) bool {
	return p.AmountMinor == in.AmountMinor &&
		p.Currency == in.Currency &&
		p.Channel == in.Channel &&
		fromPg(p.MembershipID) == payer &&
		fromPg(p.ObligationID) == in.ObligationID &&
		fromPg(p.DriveID) == in.DriveID &&
		fromPg(p.ActivityID) == in.ActivityID &&
		fromPg(p.BenefitMemberID) == in.BenefitMemberID
}

// ConfirmPayment is the officer approve step: initiated → confirmed with
// ledger credit + receipt + obligation refresh, all in one tx (Rule 12).
// The CAS itself enforces recorder != approver.
func (s *Service) ConfirmPayment(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) error {
	var p db.Payments
	err := s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.ConfirmPayment(ctx, db.ConfirmPaymentParams{
			ID: toPg(pid), OrganizationID: toPg(oc.OrgID), ApprovedBy: toPg(oc.MembershipID)})
		if err == pgx.ErrNoRows {
			// distinguish "not found" from "recorder == approver" / wrong state
			cur, e2 := q.GetPayment(ctx, db.GetPaymentParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
			if e2 != nil {
				return ErrNotFound
			}
			if cur.RecordedBy.Valid && fromPg(cur.RecordedBy) == oc.MembershipID {
				return ErrSeparation
			}
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		p = row
		return s.postConfirmation(ctx, q, oc, p, "")
	})
	return err
}

// ledgerAccount derives the ledger account from the contribution type
// behind the payment's obligation; benefits and drives get their own
// accounts; a bare manual payment is org-general.
func (s *Service) ledgerAccount(ctx context.Context, q *db.Queries, p db.Payments) string {
	if p.ObligationID.Valid {
		if ob, err := q.GetObligation(ctx, db.GetObligationParams{ID: p.ObligationID, OrganizationID: p.OrganizationID}); err == nil {
			if ct, err := q.GetContributionType(ctx, db.GetContributionTypeParams{ID: ob.ContributionTypeID, OrganizationID: ob.OrganizationID}); err == nil {
				switch ct.Key {
				case "dues", "registration":
					return "members_dues"
				case "levy":
					return "levy"
				case "welfare", "donation":
					return "welfare"
				case "project":
					return "projects"
				}
			}
		}
	}
	if p.BenefitMemberID.Valid {
		return "welfare"
	}
	if p.DriveID.Valid {
		return "levy"
	}
	return "general"
}

// postConfirmation writes the shared confirmed-payment tail: ledger
// credit, receipt, obligation refresh, and the sourced events.
func (s *Service) postConfirmation(ctx context.Context, q *db.Queries, oc *tenant.OrgCtx, p db.Payments, memo string) error {
	// Drive guard: a payment initiated while the drive was open must not
	// confirm after disbursement — the credit would land outside the
	// disbursed total, untracked by the drive's record. 'closed' still
	// confirms (money sent before the deadline); disbursed/cancelled
	// refuse — the payment stays 'initiated' for the officer to resolve.
	if p.DriveID.Valid {
		d, err := q.GetDrive(ctx, db.GetDriveParams{ID: p.DriveID, OrganizationID: p.OrganizationID})
		if err != nil {
			return err
		}
		if d.State == "disbursed" || d.State == "cancelled" {
			return ErrInvalidTransition
		}
	}
	account := s.ledgerAccount(ctx, q, p)
	le, err := q.PostLedgerEntry(ctx, db.PostLedgerEntryParams{
		OrganizationID: p.OrganizationID, PaymentID: p.ID, Direction: "credit",
		Account: account, AmountMinor: p.AmountMinor, Currency: p.Currency,
		Memo: pgText(memo), PostedBy: toPg(oc.MembershipID)})
	if err != nil {
		return err
	}
	seq, err := q.NextReceiptSeq(ctx, p.OrganizationID)
	if err != nil {
		return err
	}
	rc, err := q.CreateReceipt(ctx, db.CreateReceiptParams{
		OrganizationID: p.OrganizationID,
		PaymentID:      p.ID,
		ReceiptNo:      fmt.Sprintf("RCP-%d-%05d", time.Now().Year(), seq),
		IssuedTo:       p.MembershipID})
	if err != nil {
		return err
	}
	if err := refreshObligation(ctx, q, p.OrganizationID, p.ObligationID); err != nil {
		return err
	}
	tid, orgID := oc.TenantID, fromPg(p.OrganizationID)
	for _, ev := range []struct {
		name, aggType string
		agg           uuid.UUID
		pl            map[string]any
	}{
		{"PaymentReceived", "payment", fromPg(p.ID), map[string]any{"payment_id": fromPg(p.ID), "member_id": fromPg(p.MembershipID), "amount_minor": p.AmountMinor}},
		{"LedgerEntryPosted", "ledger_entry", fromPg(le.ID), map[string]any{"entry_id": fromPg(le.ID), "amount_minor": le.AmountMinor, "account": le.Account}},
		{"ReceiptIssued", "receipt", fromPg(rc.ID), map[string]any{"receipt_id": fromPg(rc.ID), "payment_id": fromPg(p.ID)}},
	} {
		if err := events.Emit(ctx, q, tid, orgID, ev.name, ev.aggType, ev.agg, oc.UserID, true, ev.pl); err != nil {
			return err
		}
	}
	if p.BenefitMemberID.Valid {
		return events.Emit(ctx, q, tid, orgID, "DonationReceived", "payment", fromPg(p.ID), oc.UserID, true,
			map[string]any{"payment_id": fromPg(p.ID), "donor": fromPg(p.MembershipID)})
	}
	return nil
}

// ReversePayment posts a reversing ledger entry — the original row stays
// untouched (Rule 19: corrections are reversal_of entries, never edits).
func (s *Service) ReversePayment(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID, memo string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		p, err := q.ReversePayment(ctx, db.ReversePaymentParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
		if err == pgx.ErrNoRows {
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		cred, err := q.PaymentCreditEntry(ctx, p.ID)
		var revOf pgtype.UUID
		if err == nil {
			revOf = cred.ID
		} else if err != pgx.ErrNoRows {
			return err
		}
		le, err := q.PostLedgerEntry(ctx, db.PostLedgerEntryParams{
			OrganizationID: p.OrganizationID, PaymentID: p.ID, ReversalOf: revOf,
			Direction: "debit", Account: s.ledgerAccount(ctx, q, p), AmountMinor: p.AmountMinor,
			Currency: p.Currency, Memo: pgText(memo), PostedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		if err := refreshObligation(ctx, q, p.OrganizationID, p.ObligationID); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, fromPg(p.OrganizationID), "ReversalPosted",
			"ledger_entry", fromPg(le.ID), oc.UserID, true,
			map[string]any{"entry_id": fromPg(le.ID), "reversal_of": fromPg(revOf),
				"payment_id": fromPg(p.ID), "member_id": fromPg(p.MembershipID)})
	})
}

func (s *Service) ReconcilePayment(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID, note string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		p, err := q.ReconcilePayment(ctx, db.ReconcilePaymentParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
		if err == pgx.ErrNoRows {
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		if _, err := q.CreateReconciliation(ctx, db.CreateReconciliationParams{
			OrganizationID: p.OrganizationID, PaymentID: p.ID,
			ReconciledBy: toPg(oc.MembershipID), Note: pgText(note)}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, fromPg(p.OrganizationID), "PaymentReconciled",
			"payment", fromPg(p.ID), oc.UserID, true, map[string]any{"payment_id": fromPg(p.ID)})
	})
}

// RetryPayment — billing:retry_payment: a failed payment re-enters
// 'initiated' for re-approval; confirmed money is never touched.
func (s *Service) RetryPayment(ctx context.Context, pid uuid.UUID) error {
	n, err := s.q.RetryPaymentByID(ctx, toPg(pid))
	if err == nil && n == 0 {
		err = ErrInvalidTransition
	}
	return err
}

// ---------- claims (member claim → officer verify) ----------

func (s *Service) SubmitClaim(ctx context.Context, oc *tenant.OrgCtx, in PaymentIn, note string, receiptMediaID uuid.UUID) (uuid.UUID, error) {
	if in.AmountMinor <= 0 || in.Currency == "" {
		return uuid.Nil, ErrBadValue
	}
	if in.Channel == "" {
		in.Channel = "transfer" // schema default — members report bank tx mostly
	}
	if !claimChannels[in.Channel] {
		return uuid.Nil, ErrBadChannel
	}
	if in.ObligationID != uuid.Nil {
		// Claims settle the claimant's OWN obligations — paying on behalf of
		// another member goes through the ad-hoc claim path where the
		// reviewing officer allocates the credit.
		if ob, err := s.q.GetObligation(ctx, db.GetObligationParams{
			ID: toPg(in.ObligationID), OrganizationID: toPg(oc.OrgID)}); err != nil ||
			fromPg(ob.MembershipID) != oc.MembershipID {
			return uuid.Nil, ErrNotFound
		}
	}
	if in.DriveID != uuid.Nil {
		d, err := s.q.GetDrive(ctx, db.GetDriveParams{ID: toPg(in.DriveID), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return uuid.Nil, ErrNotFound
		}
		if d.State != "open" {
			return uuid.Nil, ErrInvalidTransition
		}
		if d.Kind == "fixed_amount" && d.FixedAmountMinor.Valid && in.AmountMinor != d.FixedAmountMinor.Int64 {
			return uuid.Nil, ErrDriveKind
		}
	}
	// require_receipt for non-cash (org.finance.claims.require_receipt, default true)
	if in.Channel != "cash" && receiptMediaID == uuid.Nil {
		req := true
		if raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{
			OrganizationID: toPg(oc.OrgID), Key: "org.finance.claims.require_receipt"}); err == nil {
			_ = json.Unmarshal(raw, &req)
		}
		if req {
			return uuid.Nil, errors.New("receipt proof required for non-cash claims")
		}
	}
	var row db.PaymentClaims
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateClaim(ctx, db.CreateClaimParams{
			OrganizationID: toPg(oc.OrgID), MembershipID: toPg(oc.MembershipID),
			DriveID: toPg(in.DriveID), ObligationID: toPg(in.ObligationID),
			AmountMinor: in.AmountMinor, Currency: in.Currency,
			Channel: in.Channel, ExternalRef: pgText(in.ExternalRef),
			ReceiptMediaID: toPg(receiptMediaID), Note: pgText(note),
			HideContributor: in.HideContributor})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAlreadyClaimed // ON CONFLICT DO NOTHING — same ref already claimed
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "PaymentClaimSubmitted",
			"payment_claim", fromPg(row.ID), oc.UserID, false,
			map[string]any{"claim_id": fromPg(row.ID), "member_id": oc.MembershipID,
				"amount_minor": in.AmountMinor, "drive_id": in.DriveID})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(row.ID), nil
}

// ConfirmClaim creates the payment + ledger + receipt trail — the claim is
// the provenance record (recorded_by = claimant, approved_by = reviewer).
// Ad-hoc claims (no obligation/drive at submit) can be allocated on
// confirm: driveID/obligationID override the claim's links when non-nil
// (§12: "unallocated, officer assigns on confirm").
func (s *Service) ConfirmClaim(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, note string, driveID, obligationID uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		c, err := q.GetClaim(ctx, db.GetClaimParams{ID: toPg(cid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		if fromPg(c.MembershipID) == oc.MembershipID {
			return ErrSeparation // claimant may never verify their own claim
		}
		drive, obligation := c.DriveID, c.ObligationID
		if driveID != uuid.Nil || obligationID != uuid.Nil {
			// Officer allocation — targets must exist in this org.
			if driveID != uuid.Nil {
				if _, err := q.GetDrive(ctx, db.GetDriveParams{ID: toPg(driveID), OrganizationID: c.OrganizationID}); err != nil {
					return ErrNotFound
				}
				drive = toPg(driveID)
			}
			if obligationID != uuid.Nil {
				ob, err := q.GetObligation(ctx, db.GetObligationParams{ID: toPg(obligationID), OrganizationID: c.OrganizationID})
				if err != nil {
					return ErrNotFound
				}
				// Same ownership rule as RecordPayment: a claim settles the
				// claimant's own obligations — crediting another member's
				// debt would corrupt the who's-paid roll call.
				if fromPg(ob.MembershipID) != fromPg(c.MembershipID) {
					return ErrBadValue
				}
				obligation = toPg(obligationID)
			}
		}
		p, err := q.CreatePayment(ctx, db.CreatePaymentParams{
			OrganizationID: c.OrganizationID, MembershipID: c.MembershipID,
			ObligationID: obligation, AmountMinor: c.AmountMinor, Currency: c.Currency,
			Channel: c.Channel, ExternalRef: c.ExternalRef, DriveID: drive,
			ClaimID: c.ID, RecordedBy: c.MembershipID})
		if err != nil {
			return err
		}
		p, err = q.ConfirmPayment(ctx, db.ConfirmPaymentParams{
			ID: p.ID, OrganizationID: c.OrganizationID, ApprovedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err // claimant == reviewer hits the SoD guard in the CAS
		}
		if _, err := q.ConfirmClaim(ctx, db.ConfirmClaimParams{
			ID: c.ID, OrganizationID: c.OrganizationID, ReviewedBy: toPg(oc.MembershipID),
			ReviewNote: pgText(note), PaymentID: p.ID}); errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition
		} else if err != nil {
			return err // real failure — not a state conflict
		}
		if err := s.postConfirmation(ctx, q, oc, p, "claim:"+c.ID.String()[:8]); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "PaymentClaimConfirmed",
			"payment_claim", cid, oc.UserID, true,
			map[string]any{"claim_id": cid, "payment_id": fromPg(p.ID), "reviewer_id": oc.MembershipID})
	})
}

func (s *Service) RejectClaim(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, note string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.RejectClaim(ctx, db.RejectClaimParams{
			ID: toPg(cid), OrganizationID: toPg(oc.OrgID), ReviewedBy: toPg(oc.MembershipID),
			ReviewNote: pgText(note)}); errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition
		} else if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "PaymentClaimRejected",
			"payment_claim", cid, oc.UserID, false,
			map[string]any{"claim_id": cid, "reviewer_id": oc.MembershipID, "reason": note})
	})
}

func (s *Service) ListClaims(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.PaymentClaims, error) {
	return s.q.ListClaims(ctx, db.ListClaimsParams{OrganizationID: toPg(oc.OrgID), Column2: state})
}

func (s *Service) MyClaims(ctx context.Context, userID uuid.UUID) ([]db.PaymentClaims, error) {
	return s.q.MyClaimsAll(ctx, toPg(userID))
}

// ---------- drives ----------

type DriveIn struct {
	Title              string    `json:"title"`
	Purpose            string    `json:"purpose"`
	Kind               string    `json:"kind"`
	FixedAmountMinor   int64     `json:"fixed_amount_minor"`
	TargetAmountMinor  int64     `json:"target_amount_minor"`
	BenefitMemberID    uuid.UUID `json:"benefit_member_id"`
	ContributionTypeID uuid.UUID `json:"contribution_type_id"`
	ClosesAt           time.Time `json:"closes_at"`
}

func (s *Service) OpenDrive(ctx context.Context, oc *tenant.OrgCtx, in DriveIn) (uuid.UUID, error) {
	if in.Title == "" {
		return uuid.Nil, errors.New("title required")
	}
	if in.Kind == "fixed_amount" && in.FixedAmountMinor <= 0 {
		return uuid.Nil, ErrDriveKind
	}
	// Linked rows must be THIS org's — the FKs only prove existence.
	if in.BenefitMemberID != uuid.Nil {
		if org, err := s.q.GetMembershipOrg(ctx, toPg(in.BenefitMemberID)); err != nil || fromPg(org) != oc.OrgID {
			return uuid.Nil, ErrNotFound
		}
	}
	if in.ContributionTypeID != uuid.Nil {
		if _, err := s.q.GetContributionType(ctx, db.GetContributionTypeParams{
			ID: toPg(in.ContributionTypeID), OrganizationID: toPg(oc.OrgID)}); err != nil {
			return uuid.Nil, ErrNotFound
		}
	}
	var row db.ContributionDrives
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateDrive(ctx, db.CreateDriveParams{
			OrganizationID: toPg(oc.OrgID), Title: in.Title, Purpose: pgText(in.Purpose),
			Kind: in.Kind, FixedAmountMinor: pgtype.Int8{Int64: in.FixedAmountMinor, Valid: in.FixedAmountMinor > 0},
			TargetAmountMinor: pgtype.Int8{Int64: in.TargetAmountMinor, Valid: in.TargetAmountMinor > 0},
			BenefitMemberID:   toPg(in.BenefitMemberID), ContributionTypeID: toPg(in.ContributionTypeID),
			ClosesAt:  pgtype.Timestamptz{Time: in.ClosesAt, Valid: !in.ClosesAt.IsZero()},
			CreatedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ContributionDriveOpened",
			"contribution_drive", fromPg(row.ID), oc.UserID, true,
			map[string]any{"drive_id": fromPg(row.ID), "kind": in.Kind,
				"target_minor": in.TargetAmountMinor, "benefit_member_id": in.BenefitMemberID})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(row.ID), nil
}

func (s *Service) ListDrives(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.ContributionDrives, error) {
	return s.q.ListDrives(ctx, db.ListDrivesParams{OrganizationID: toPg(oc.OrgID), Column2: state})
}

func (s *Service) DriveDetail(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID) (*db.ContributionDrives, int64, int32, error) {
	d, err := s.q.GetDrive(ctx, db.GetDriveParams{ID: toPg(did), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, 0, 0, ErrNotFound
	}
	t, err := s.q.DriveCollected(ctx, d.ID)
	return &d, t.CollectedMinor, t.Contributors, err
}

// PatchDrive edits an open drive (drives.manage PATCH in the catalog) —
// closed/disbursed drives are immutable; nil fields keep their values.
func (s *Service) PatchDrive(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID, title, purpose *string, closesAt *time.Time) error {
	var t, p pgtype.Text
	if title != nil {
		t = pgText(*title)
	}
	if purpose != nil {
		p = pgText(*purpose)
	}
	var ca pgtype.Timestamptz // nil pointer = keep closes_at
	if closesAt != nil {
		ca = pgtype.Timestamptz{Time: *closesAt, Valid: true}
	}
	n, err := s.q.UpdateDrive(ctx, db.UpdateDriveParams{
		ID: toPg(did), OrganizationID: toPg(oc.OrgID), Title: t, Purpose: p,
		ClosesAt: ca})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalidTransition
	}
	return nil
}

// DriveContribution is the contributor-facing row — membership_id is
// masked when the org hides contributors or the claimant opted out
// (org.finance.drives.show_contributors; per-claim hide_contributor).
type DriveContribution struct {
	PaymentID    uuid.UUID  `json:"payment_id"`
	MembershipID *uuid.UUID `json:"membership_id,omitempty"`
	AmountMinor  int64      `json:"amount_minor"`
	Currency     string     `json:"currency"`
	Channel      string     `json:"channel"`
	ConfirmedAt  time.Time  `json:"confirmed_at"`
}

// DriveContributions — canReview = caller holds claims.review or
// drives.manage: reviewers always see full provenance.
func (s *Service) DriveContributions(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID, canReview bool) ([]DriveContribution, error) {
	d, err := s.q.GetDrive(ctx, db.GetDriveParams{ID: toPg(did), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.q.DriveContributionRows(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	show := true
	if !canReview {
		show = s.orgSettingBool(ctx, oc.OrgID, "org.finance.drives.show_contributors", true)
	}
	out := make([]DriveContribution, 0, len(rows))
	for _, r := range rows {
		c := DriveContribution{
			PaymentID: fromPg(r.ID), AmountMinor: r.AmountMinor,
			Currency: r.Currency, Channel: r.Channel, ConfirmedAt: r.ConfirmedAt.Time,
		}
		hidden := r.HideContributor.Valid && r.HideContributor.Bool
		if canReview || (show && !hidden) {
			mid := fromPg(r.MembershipID)
			c.MembershipID = &mid
		}
		out = append(out, c)
	}
	return out, nil
}

func (s *Service) CloseDrive(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetDriveState(ctx, db.SetDriveStateParams{
			ID: toPg(did), OrganizationID: toPg(oc.OrgID), State: "closed", Column4: "open"})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrInvalidTransition
		}
		tot, _ := q.DriveCollected(ctx, toPg(did))
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ContributionDriveClosed",
			"contribution_drive", did, oc.UserID, true,
			map[string]any{"drive_id": did, "total_collected_minor": tot.CollectedMinor})
	})
}

// DisburseDrive releases collected funds — an expense (debit) ledger entry,
// never a deletion or a hidden transfer.
func (s *Service) DisburseDrive(ctx context.Context, oc *tenant.OrgCtx, did uuid.UUID, memo string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetDriveState(ctx, db.SetDriveStateParams{
			ID: toPg(did), OrganizationID: toPg(oc.OrgID), State: "disbursed", Column4: "closed"})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrInvalidTransition
		}
		tot, _ := q.DriveCollected(ctx, toPg(did))
		cur := "NGN"
		if pays, err := q.DrivePayments(ctx, toPg(did)); err == nil && len(pays) > 0 {
			cur = pays[0].Currency
		}
		var le db.LedgerEntries
		if tot.CollectedMinor > 0 {
			var err error
			le, err = q.PostLedgerEntry(ctx, db.PostLedgerEntryParams{
				OrganizationID: toPg(oc.OrgID), Direction: "debit", Account: "general",
				AmountMinor: tot.CollectedMinor, Currency: cur,
				Memo: pgText(memo), PostedBy: toPg(oc.MembershipID)})
			if err != nil {
				return err
			}
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ContributionDisbursed",
			"contribution_drive", did, oc.UserID, true,
			map[string]any{"drive_id": did, "expense_entry_id": fromPg(le.ID)})
	})
}

// ---------- sweeps + dues_standing ----------

// OverdueSweep (finance:overdue_sweep): flips grace-expired obligations to
// overdue (per-org org.finance.grace_days via the query), recomputes
// dues_standing for affected members, and flags defaulters for comms.
func (s *Service) OverdueSweep(ctx context.Context, graceDays int) (int, error) {
	rows, err := s.q.MarkObligationsOverdue(ctx, int32(graceDays))
	if err != nil {
		return 0, err
	}
	seen := map[uuid.UUID]uuid.UUID{} // membership → org
	for _, r := range rows {
		seen[fromPg(r.MembershipID)] = fromPg(r.OrganizationID)
	}
	tenantCache := map[uuid.UUID]uuid.UUID{}
	for mid, orgID := range seen {
		tid, ok := tenantCache[orgID]
		if !ok {
			if t, err := s.q.GetOrgTenantID(ctx, toPg(orgID)); err == nil {
				tid = fromPg(t)
			}
			tenantCache[orgID] = tid
		}
		// Read standing, recompute, and emit the flag in ONE tx — a failed
		// emit rolls the standing back so the next tick retries the flag
		// instead of silently losing the good→poor transition.
		err := s.withTx(ctx, func(q *db.Queries) error {
			before := "good"
			if st, err := q.GetMemberStanding(ctx, toPg(mid)); err == nil {
				before = st
			}
			if err := s.RefreshDuesStanding(ctx, q, orgID, mid); err != nil {
				return err
			}
			if before != "good" {
				return nil // already flagged — don't re-flag every tick
			}
			// Defaulter flag for comms — the consumer-facing event carries
			// the member + how many obligations went overdue.
			var count int
			if obs, err := q.OpenObligationsForMember(ctx, toPg(mid)); err == nil {
				for _, o := range obs {
					if o.Status == "overdue" {
						count++
					}
				}
			}
			return events.Emit(ctx, q, tid, orgID, "DuesDefaulterFlagged",
				"organization_membership", mid, uuid.Nil, true,
				map[string]any{"membership_id": mid, "count": count})
		})
		if err != nil {
			return len(rows), err
		}
		// §12: optional defaulter fine (org.finance.defaulter_fine_minor) —
		// a penalty-kind obligation minted once per good→poor transition,
		// in the currency of the member's overdue obligation.
		if fine := s.orgSettingInt(ctx, orgID, "org.finance.defaulter_fine_minor", 0); fine > 0 {
			cur := "NGN"
			if obs, err := s.q.OpenObligationsForMember(ctx, toPg(mid)); err == nil {
				for _, o := range obs {
					if o.Status == "overdue" {
						cur = o.Currency
						break
					}
				}
			}
			s.assessFine(ctx, tid, orgID, mid, fine, cur, PeriodLabel("monthly", time.Now()))
		}
	}
	return len(rows), nil
}

// orgSettingInt / orgSettingBool — org_settings lookups with catalog
// defaults (Rule 3/23: org rules resolve through settings).
func (s *Service) orgSettingInt(ctx context.Context, orgID uuid.UUID, key string, fallback int64) int64 {
	raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{OrganizationID: toPg(orgID), Key: key})
	if err != nil {
		return fallback
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return fallback
	}
	if v, err := n.Int64(); err == nil {
		return v
	}
	return fallback
}

func (s *Service) orgSettingBool(ctx context.Context, orgID uuid.UUID, key string, fallback bool) bool {
	raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{OrganizationID: toPg(orgID), Key: key})
	if err != nil {
		return fallback
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		return fallback
	}
	return v
}

// assessFine posts a penalty obligation: ensures the org's 'fine'
// contribution type, then mints the obligation + FineAssessed in one tx.
// Idempotent — the (member,type,period) unique dedupes repeats of the
// same period ("attendance:{activity}" scopes per meeting; "YYYY-MM"
// scopes the monthly defaulter fine).
func (s *Service) assessFine(ctx context.Context, tid, orgID, mid uuid.UUID, amount int64, currency, period string) {
	if err := s.withTx(ctx, func(q *db.Queries) error {
		ct, err := q.EnsureContributionType(ctx, db.EnsureContributionTypeParams{
			OrganizationID: toPg(orgID), Key: "fine", Label: "Defaulter fine", Kind: "penalty"})
		if err != nil {
			return err
		}
		ob, err := q.CreateObligation(ctx, db.CreateObligationParams{
			OrganizationID: toPg(orgID), MembershipID: toPg(mid),
			ContributionTypeID: ct, PeriodLabel: pgText(period),
			AmountMinor: amount, Currency: currency,
			DueDate: pgDate(time.Now().Add(30 * 24 * time.Hour))})
		if err == pgx.ErrNoRows {
			// Fine already exists for this period — a re-mark between fined
			// statuses (late<->absent) adjusts the open fine to the new
			// amount rather than silently keeping the stale one.
			_, err := q.AdjustFineForPeriod(ctx, db.AdjustFineForPeriodParams{
				MembershipID: toPg(mid), OrganizationID: toPg(orgID),
				PeriodLabel: pgText(period), AmountMinor: amount})
			return err
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, tid, orgID, "FineAssessed", "obligation",
			fromPg(ob.ID), uuid.Nil, true,
			map[string]any{"obligation_id": fromPg(ob.ID), "member_id": mid, "kind": "penalty"})
	}); err != nil {
		// post-commit seam: a failed fine write must not be silent — the
		// attendance sheet already committed, so log loudly for the sweep
		// of durable AttendanceRecorded events to reconcile.
		slog.Error("assess_fine_failed", "org", orgID, "member", mid, "period", period, "err", err)
	}
}

// RefreshDuesStanding derives good/poor from live overdue obligations —
// 'good' when nothing is overdue (§12: recomputed by the sweep + consumers).
func (s *Service) RefreshDuesStanding(ctx context.Context, q *db.Queries, orgID, membershipID uuid.UUID) error {
	obs, err := q.OpenObligationsForMember(ctx, toPg(membershipID))
	if err != nil {
		return err
	}
	standing := "good"
	for _, o := range obs {
		if o.Status == "overdue" {
			standing = "poor"
			break
		}
	}
	_, err = q.SetDuesStanding(ctx, db.SetDuesStandingParams{
		ID: toPg(membershipID), OrganizationID: toPg(orgID), DuesStanding: standing})
	return err
}

// DuesStandingConsumer reacts to payment/obligation events — any money
// moving on a member re-derives their standing (at-least-once safe: the
// recompute is idempotent).
func (s *Service) DuesStandingConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	switch e.Type {
	case "PaymentReceived", "ObligationCreated", "DuesDefaulterFlagged",
		"ObligationWaived", "ReversalPosted", "PaymentClaimConfirmed":
	default:
		return nil
	}
	var mid uuid.UUID
	if v, ok := e.Payload["member_id"].(string); ok {
		mid, _ = uuid.Parse(v)
	}
	if mid == uuid.Nil {
		if v, ok := e.Payload["membership_id"].(string); ok {
			mid, _ = uuid.Parse(v)
		}
	}
	if mid == uuid.Nil {
		if v, ok := e.Payload["payment_id"].(string); ok {
			pid, _ := uuid.Parse(v)
			if p, err := q.GetPayment(ctx, db.GetPaymentParams{ID: toPg(pid), OrganizationID: toPg(e.OrganizationID)}); err == nil {
				mid = fromPg(p.MembershipID)
			}
		}
	}
	if mid == uuid.Nil {
		if v, ok := e.Payload["obligation_id"].(string); ok {
			oid, _ := uuid.Parse(v)
			if ob, err := q.GetObligation(ctx, db.GetObligationParams{ID: toPg(oid), OrganizationID: toPg(e.OrganizationID)}); err == nil {
				mid = fromPg(ob.MembershipID)
			}
		}
	}
	if mid == uuid.Nil {
		return nil
	}
	return s.RefreshDuesStanding(ctx, q, e.OrganizationID, mid)
}

// EnsureReceipt is the finance:issue_receipt retry path — a confirmed
// payment gets its receipt if the confirm-tx insert ever missed (the
// payment-unique index keeps it idempotent).
func (s *Service) EnsureReceipt(ctx context.Context, pid uuid.UUID) error {
	p, err := s.q.GetPaymentAny(ctx, toPg(pid))
	if err != nil {
		return ErrNotFound
	}
	if p.Status != "confirmed" && p.Status != "reconciled" {
		return ErrInvalidTransition
	}
	if _, err := s.q.ReceiptForPayment(ctx, p.ID); err == nil {
		return nil // already issued
	}
	var tenantID uuid.UUID
	if t, err := s.q.GetOrgTenantID(ctx, p.OrganizationID); err == nil {
		tenantID = fromPg(t)
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		seq, err := q.NextReceiptSeq(ctx, p.OrganizationID)
		if err != nil {
			return err
		}
		rc, err := q.CreateReceipt(ctx, db.CreateReceiptParams{
			OrganizationID: p.OrganizationID,
			PaymentID:      p.ID,
			ReceiptNo:      fmt.Sprintf("RCP-%d-%05d", time.Now().Year(), seq),
			IssuedTo:       p.MembershipID})
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, tenantID, fromPg(p.OrganizationID), "ReceiptIssued",
			"receipt", fromPg(rc.ID), uuid.Nil, true,
			map[string]any{"receipt_id": fromPg(rc.ID), "payment_id": fromPg(p.ID)})
	})
}

// CloseExpiredDrives — finance:drive_closing_sweep: open drives past
// closes_at flip to closed (emits per-drive events from the row org).
func (s *Service) CloseExpiredDrives(ctx context.Context) error {
	rows, err := s.q.ExpiredOpenDrives(ctx)
	if err != nil {
		return err
	}
	for _, d := range rows {
		var tenantID uuid.UUID
		if t, err := s.q.GetOrgTenantID(ctx, d.OrganizationID); err == nil {
			tenantID = fromPg(t)
		}
		_ = s.withTx(ctx, func(q *db.Queries) error {
			if n, err := q.SetDriveState(ctx, db.SetDriveStateParams{
				ID: d.ID, OrganizationID: d.OrganizationID, State: "closed", Column4: "open"}); err != nil || n == 0 {
				return err
			}
			tot, _ := q.DriveCollected(ctx, d.ID)
			return events.Emit(ctx, q, tenantID, fromPg(d.OrganizationID), "ContributionDriveClosed",
				"contribution_drive", fromPg(d.ID), uuid.Nil, true,
				map[string]any{"drive_id": fromPg(d.ID), "total_collected_minor": tot.CollectedMinor})
		})
	}
	return nil
}

// ---------- registration (ADR-014 approved→registered guard) ----------

// RegistrationObligationConsumer is the outbox consumer for §11 test
// scenario 2: MemberApproved auto-creates the registration obligation —
// one per active 'registration' schedule, idempotent on the
// (member,type,period) unique.
func (s *Service) RegistrationObligationConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "MemberApproved" || e.AggregateType != "organization_membership" {
		return nil
	}
	mid := e.AggregateID
	if mid == uuid.Nil {
		if v, ok := e.Payload["membership_id"].(string); ok {
			mid, _ = uuid.Parse(v)
		}
	}
	if mid == uuid.Nil {
		return nil
	}
	scheds, err := q.RegistrationSchedules(ctx, toPg(e.OrganizationID))
	if err != nil {
		return err
	}
	for _, sc := range scheds {
		ob, err := q.CreateObligation(ctx, db.CreateObligationParams{
			OrganizationID: sc.OrganizationID, MembershipID: toPg(mid),
			ContributionTypeID: sc.ContributionTypeID, ScheduleID: sc.ID,
			PeriodLabel: pgText("registration"), AmountMinor: sc.AmountMinor,
			Currency: sc.Currency, DueDate: sc.StartsOn})
		if err == pgx.ErrNoRows {
			continue // already minted — idempotent consumer replay
		}
		if err != nil {
			return err
		}
		if err := events.Emit(ctx, q, e.TenantID, e.OrganizationID,
			"ObligationCreated", "obligation", fromPg(ob.ID), uuid.Nil, true,
			map[string]any{"obligation_id": fromPg(ob.ID), "member_id": mid,
				"amount_minor": ob.AmountMinor}); err != nil {
			return err
		}
	}
	return nil
}

// RegistrationPending — the approved→registered guard (ADR-014): true when
// the member still owes registration money. A gate for the member
// lifecycle — wired via tenant.SetRegistrationGate (module seam: the
// members package can't read finance tables, Rule 26).
func (s *Service) RegistrationPending(ctx context.Context, orgID, memberID uuid.UUID) (bool, error) {
	n, err := s.q.UnpaidRegistrationObligations(ctx, db.UnpaidRegistrationObligationsParams{
		OrganizationID: toPg(orgID), MembershipID: toPg(memberID)})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// StatementVisibility resolves org.finance.statement_visibility
// (members|exco, default members) — the handler upgrades 'exco' to a
// reports.read gate.
func (s *Service) StatementVisibility(ctx context.Context, orgID uuid.UUID) string {
	raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{
		OrganizationID: toPg(orgID), Key: "org.finance.statement_visibility"})
	if err == nil {
		var v string
		if json.Unmarshal(raw, &v) == nil && (v == "exco" || v == "members") {
			return v
		}
	}
	return "members"
}

// ---------- reads ----------

// ObligationsStatus — /finance/obligations/status + /schedules/{sid}/status
// roll call: per-member paid state, filterable by schedule/period/type.
func (s *Service) ObligationsStatus(ctx context.Context, oc *tenant.OrgCtx, scheduleID uuid.UUID, period string, typeID uuid.UUID) ([]db.ObligationRollCallRow, error) {
	return s.q.ObligationRollCall(ctx, db.ObligationRollCallParams{
		OrganizationID: toPg(oc.OrgID), Column2: toPg(scheduleID),
		Column3: period, Column4: toPg(typeID)})
}

// Contributions — /finance/contributions: per-type contribution history.
func (s *Service) Contributions(ctx context.Context, oc *tenant.OrgCtx) ([]db.ContributionTotalsRow, error) {
	return s.q.ContributionTotals(ctx, toPg(oc.OrgID))
}

// Donations — /activities/{aid}/donations: event-linked confirmed money.
func (s *Service) Donations(ctx context.Context, oc *tenant.OrgCtx, activityID uuid.UUID) ([]db.Payments, error) {
	return s.q.ActivityDonations(ctx, db.ActivityDonationsParams{
		OrganizationID: toPg(oc.OrgID), ActivityID: toPg(activityID)})
}

// UpdateSchedule — the catalog PATCH surface: nullable fields keep their
// values; explicit ends_on:null clears the end date (clearEndsOn).
func (s *Service) UpdateSchedule(ctx context.Context, oc *tenant.OrgCtx, sid uuid.UUID, in ScheduleIn, active *bool, clearEndsOn bool) error {
	n, err := s.q.UpdateDuesSchedule(ctx, db.UpdateDuesScheduleParams{
		ID: toPg(sid), OrganizationID: toPg(oc.OrgID),
		Name:        pgtype.Text{String: in.Name, Valid: in.Name != ""},
		AmountMinor: pgtype.Int8{Int64: in.AmountMinor, Valid: in.AmountMinor > 0},
		Recurrence:  pgtype.Text{String: in.Recurrence, Valid: in.Recurrence != ""},
		AppliesTo:   in.AppliesTo,
		StartsOn:    pgDate(in.StartsOn),
		EndsOn:      pgDate(in.EndsOn),
		Active:      pgtype.Bool{Bool: active != nil && *active, Valid: active != nil},
		ClearEndsOn: clearEndsOn,
	})
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

func (s *Service) Summary(ctx context.Context, oc *tenant.OrgCtx) (db.OrgPaymentTotalsRow, error) {
	return s.q.OrgPaymentTotals(ctx, toPg(oc.OrgID))
}

func (s *Service) Ledger(ctx context.Context, oc *tenant.OrgCtx, account string) ([]db.LedgerEntries, error) {
	return s.q.ListLedgerEntries(ctx, db.ListLedgerEntriesParams{OrganizationID: toPg(oc.OrgID), Column2: account})
}

// Statement — §12 org purse: current balance, monthly + yearly
// inflow/outflow, and the per-account expense/revenue breakdown.
type Statement struct {
	BalanceMinor      int64                   `json:"balance_minor"`
	MonthInflowMinor  int64                   `json:"month_inflow_minor"`
	MonthOutflowMinor int64                   `json:"month_outflow_minor"`
	YearInflowMinor   int64                   `json:"year_inflow_minor"`
	YearOutflowMinor  int64                   `json:"year_outflow_minor"`
	ByAccount         []db.OrgLedgerTotalsRow `json:"by_account"`
}

func (s *Service) Statement(ctx context.Context, oc *tenant.OrgCtx) (*Statement, error) {
	t, err := s.q.OrgLedgerTimeTotals(ctx, toPg(oc.OrgID))
	if err != nil {
		return nil, err
	}
	accts, err := s.q.OrgLedgerTotals(ctx, toPg(oc.OrgID))
	if err != nil {
		return nil, err
	}
	return &Statement{
		BalanceMinor:      t.CreditMinor - t.DebitMinor,
		MonthInflowMinor:  t.MonthCreditMinor,
		MonthOutflowMinor: t.MonthDebitMinor,
		YearInflowMinor:   t.YearCreditMinor,
		YearOutflowMinor:  t.YearDebitMinor,
		ByAccount:         accts,
	}, nil
}

func (s *Service) Defaulters(ctx context.Context, oc *tenant.OrgCtx) ([]db.MembersWithOverdueObligationsRow, error) {
	return s.q.MembersWithOverdueObligations(ctx, toPg(oc.OrgID))
}

// MyFinance is the cross-org member view (/me/finance) — obligations and
// payments across every membership the user holds.
func (s *Service) MyFinance(ctx context.Context, userID uuid.UUID) ([]db.MyFinanceFeedRow, error) {
	return s.q.MyFinanceFeed(ctx, toPg(userID))
}

func (s *Service) GetReceipt(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID) (*db.Receipts, error) {
	r, err := s.q.GetReceiptByIDOrg(ctx, db.GetReceiptByIDOrgParams{ID: toPg(rid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, ErrNotFound
	}
	return &r, nil
}

// AssessFine is the Rule-26 seam other modules use to post penalties
// (§13 attendance fines): a penalty-kind obligation + FineAssessed event,
// deduped per (member, fine type, period).
func (s *Service) AssessFine(ctx context.Context, orgID, memberID uuid.UUID, amount int64, currency, period string) {
	if amount <= 0 {
		return
	}
	s.assessFine(ctx, uuid.Nil, orgID, memberID, amount, currency, period)
}

// RevokeFine is the Rule-26 seam other modules use to withdraw a penalty
// when its trigger is corrected (§13: a re-marked attendance must not keep
// the fine). Waives open 'fine' obligations for the member+period —
// already-paid fines stay (a refund is a separate decision).
func (s *Service) RevokeFine(ctx context.Context, orgID, memberID uuid.UUID, period string) {
	if err := s.withTx(ctx, func(q *db.Queries) error {
		ids, err := q.WaiveFineForPeriod(ctx, db.WaiveFineForPeriodParams{
			MembershipID: toPg(memberID), OrganizationID: toPg(orgID),
			PeriodLabel: pgtype.Text{String: period, Valid: true}})
		if err != nil {
			return err
		}
		for _, id := range ids {
			if err := events.Emit(ctx, q, uuid.Nil, orgID, "ObligationWaived",
				"obligation", fromPg(id), uuid.Nil, true,
				map[string]any{"obligation_id": fromPg(id), "reason": "mark_corrected"}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		slog.Error("revoke_fine_failed", "org", orgID, "member", memberID, "period", period, "err", err)
	}
}
