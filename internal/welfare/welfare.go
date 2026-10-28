// Ref: oga9-engineering/sections/17-welfare.md — welfare cases: eligibility
// (org_settings rules, Rule 23), approvals (cap → EXCO-level threshold),
// disbursement (ledger via BenefitDisbursed consumer), visits, history.
package welfare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/rules"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrBadValue   = errors.New("bad value")
	ErrBadState   = errors.New("bad_case_state")
	ErrIneligible = errors.New("welfare_ineligible")
	ErrDuplicate  = errors.New("already_decided")
	ErrSelfAction = errors.New("welfare_self_action")
)

var welfareKinds = map[string]bool{"medical": true, "bereavement": true, "marriage": true,
	"childbirth": true, "emergency": true, "scholarship": true, "graduation": true, "business_opening": true}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }
func nInt(v int64) pgtype.Int8       { return pgtype.Int8{Int64: v, Valid: v != 0} }

type Service struct {
	ru   *rules.Service // ADR-016 rules engine — org_settings resolve through it (Rule 23)
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{ru: rules.New(pool), pool: pool, q: db.New(pool)}
}

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// settingInt reads an org.welfare.* numeric rule (Rule 23 — org_settings,
// never hardcoded). Missing/odd values fall back to the default.
func (s *Service) settingInt(ctx context.Context, orgID uuid.UUID, key string, def int64) int64 {
	raw, err := s.ru.Value(ctx, orgID, key)
	if err != nil {
		return def
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return def
	}
	switch t := v.(type) {
	case float64:
		return int64(t)
	case string:
		var n int64
		if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
			return n
		}
	}
	return def
}

func (s *Service) settingBool(ctx context.Context, orgID uuid.UUID, key string) bool {
	raw, err := s.ru.Value(ctx, orgID, key)
	if err != nil {
		return false
	}
	var v bool
	if json.Unmarshal(raw, &v) == nil {
		return v
	}
	var sv string
	if json.Unmarshal(raw, &sv) == nil {
		return sv == "true" || sv == "1"
	}
	return false
}

func (s *Service) auditCase(ctx context.Context, q *db.Queries, caseID, actor uuid.UUID, action, note string) error {
	_, err := q.AddCaseEvent(ctx, db.AddCaseEventParams{
		CaseID: toPg(caseID), ActorID: toPg(actor), Action: action, Note: pgText(note)})
	return err
}

// ---------- cases ----------

// CreateCase — member requests support. Eligibility is rules-driven:
// min_membership_days, requires_good_standing, once_per.{kind}_days (a
// repeat inside the window is flagged for reviewers, not auto-rejected).
func (s *Service) CreateCase(ctx context.Context, oc *tenant.OrgCtx, kind, title, details string,
	amountMinor int64, currency string) (uuid.UUID, error) {
	if !welfareKinds[kind] || title == "" {
		return uuid.Nil, ErrBadValue
	}
	m, err := s.q.GetMembershipByID(ctx, toPg(oc.MembershipID))
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	if days := s.settingInt(ctx, oc.OrgID, "org.welfare.min_membership_days", 0); days > 0 {
		since := m.CreatedAt.Time
		if m.JoinedAt.Valid {
			since = m.JoinedAt.Time
		}
		if time.Since(since) < time.Duration(days)*24*time.Hour {
			return uuid.Nil, ErrIneligible
		}
	}
	// "good standing" is dues standing (§11/§22 semantics — the same
	// notion §21 eligibility uses), not lifecycle state.
	if s.settingBool(ctx, oc.OrgID, "org.welfare.requires_good_standing") && m.DuesStanding != "good" {
		return uuid.Nil, ErrIneligible
	}
	var repeatNote string
	if days := s.settingInt(ctx, oc.OrgID, "org.welfare.once_per."+kind+"_days", 0); days > 0 {
		if _, err := s.q.PriorCaseInWindow(ctx, db.PriorCaseInWindowParams{
			MembershipID: toPg(oc.MembershipID), Kind: kind, Days: int32(days)}); err == nil {
			repeatNote = fmt.Sprintf("repeat request inside %dd window", days)
		}
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		cid, err := q.CreateWelfareCase(ctx, db.CreateWelfareCaseParams{
			OrgID: toPg(oc.OrgID), MembershipID: toPg(oc.MembershipID), Kind: kind,
			Title: title, Details: pgText(details), RequestedAmountMinor: nInt(amountMinor),
			Currency: pgText(currency), AutoSuggested: false})
		if err != nil {
			return err
		}
		id = fromPg(cid)
		if err := s.auditCase(ctx, q, id, oc.MembershipID, "requested", repeatNote); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "WelfareRequested",
			"welfare_case", id, oc.UserID, true,
			map[string]any{"case_id": id, "member_id": oc.MembershipID, "type": kind})
	})
	return id, err
}

func (s *Service) ListCases(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.WelfareCases, error) {
	return s.q.ListWelfareCases(ctx, db.ListWelfareCasesParams{OrgID: toPg(oc.OrgID), State: pgText(state)})
}

func (s *Service) MyCases(ctx context.Context, userID uuid.UUID) ([]db.WelfareCases, error) {
	return s.q.MyWelfareCases(ctx, toPg(userID))
}

func (s *Service) GetCase(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID) (db.WelfareCases, error) {
	c, err := s.q.GetWelfareCase(ctx, db.GetWelfareCaseParams{ID: toPg(cid), OrgID: toPg(oc.OrgID)})
	if err != nil {
		return c, ErrNotFound
	}
	return c, nil
}

func (s *Service) CaseHistory(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID) ([]db.WelfareCaseEvents, error) {
	if _, err := s.GetCase(ctx, oc, cid); err != nil {
		return nil, err
	}
	return s.q.ListCaseEvents(ctx, toPg(cid))
}

// Review moves requested → reviewing (PATCH semantics: the officer picks
// the case up).
func (s *Service) Review(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, note string) error {
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return err
	}
	if c.State != "requested" {
		return ErrBadState
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetWelfareCaseState(ctx, db.SetWelfareCaseStateParams{
			ID: c.ID, ToState: "reviewing", FromState: "requested"})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState
		}
		if err := s.auditCase(ctx, q, cid, oc.MembershipID, "reviewing", note); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "WelfareReviewed",
			"welfare_case", cid, oc.UserID, true,
			map[string]any{"case_id": cid, "member_id": fromPg(c.MembershipID)})
	})
}

// Decide records an approval/rejection. §16-style cap threshold: requests
// at/under org.welfare.cap.{kind}_minor need ONE approval; above the cap
// they need TWO distinct approvers (the "EXCO-level" tier). A rejection
// closes immediately.
func (s *Service) Decide(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID,
	approve bool, amountMinor int64, currency, note string) (bool, error) {
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return false, err
	}
	if c.State != "requested" && c.State != "reviewing" {
		return false, ErrBadState
	}
	// §12 separation-of-duties parity with payments/expenses: the claimant
	// may never decide their own case, however many nodes they hold.
	if fromPg(c.MembershipID) == oc.MembershipID {
		return false, ErrSelfAction
	}
	seen, err := s.q.ApproverAlreadyDecided(ctx, db.ApproverAlreadyDecidedParams{
		CaseID: c.ID, ApproverID: toPg(oc.MembershipID)})
	if err != nil {
		return false, err
	}
	if seen {
		return false, ErrDuplicate
	}
	amount := amountMinor
	if amount == 0 && c.RequestedAmountMinor.Valid {
		amount = c.RequestedAmountMinor.Int64
	}
	cap := s.settingInt(ctx, oc.OrgID, "org.welfare.cap."+c.Kind+"_minor", 0)
	required := int64(1)
	if cap > 0 && amount > cap {
		required = 2 // above cap → EXCO-level: two distinct approvers
	}
	decision := "rejected"
	if approve {
		decision = "approved"
	}
	var approved bool
	err = s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.AddApproval(ctx, db.AddApprovalParams{
			CaseID: c.ID, ApproverID: toPg(oc.MembershipID), Decision: decision,
			ApprovedAmountMinor: nInt(amount), Currency: pgText(currency)}); err != nil {
			return err
		}
		if !approve {
			if _, err := q.SetWelfareCaseState(ctx, db.SetWelfareCaseStateParams{
				ID: c.ID, ToState: "rejected", FromState: c.State}); err != nil {
				return err
			}
			if err := s.auditCase(ctx, q, cid, oc.MembershipID, "rejected", note); err != nil {
				return err
			}
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "SupportRejected",
				"welfare_case", cid, oc.UserID, true,
				map[string]any{"case_id": cid, "member_id": fromPg(c.MembershipID), "note": note})
		}
		n, err := q.CountCaseApprovals(ctx, c.ID)
		if err != nil {
			return err
		}
		if n < required {
			// awaiting the second (EXCO-level) approver — stays reviewing.
			n2, err := q.SetWelfareCaseState(ctx, db.SetWelfareCaseStateParams{
				ID: c.ID, ToState: "reviewing", FromState: c.State})
			if err == nil && n2 == 0 && c.State != "reviewing" {
				return ErrBadState
			}
			return s.auditCase(ctx, q, cid, oc.MembershipID, "approval_pending",
				fmt.Sprintf("first approval recorded; needs %d", required))
		}
		if _, err := q.SetWelfareCaseState(ctx, db.SetWelfareCaseStateParams{
			ID: c.ID, ToState: "approved", FromState: c.State}); err != nil {
			return err
		}
		// §17: approval materializes the expense request — a pending
		// benefit row (no ledger write yet; disburse fulfills it and the
		// BenefitDisbursed consumer posts the ledger entry, Rule 19).
		if _, err := q.AddPendingBenefit(ctx, db.AddPendingBenefitParams{
			CaseID: c.ID, Kind: "cash",
			AmountMinor: nInt(amount), Currency: pgText(currency)}); err != nil {
			return err
		}
		approved = true
		if err := s.auditCase(ctx, q, cid, oc.MembershipID, "approved", note); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "SupportApproved",
			"welfare_case", cid, oc.UserID, true,
			map[string]any{"case_id": cid, "member_id": fromPg(c.MembershipID),
				"amount_minor": amount, "currency": currency})
	})
	return approved, err
}

// ---------- disbursement ----------

// Disburse records a benefit — cash disbursals carry amount+currency and
// emit BenefitDisbursed; the finance consumer posts the debit ledger
// entry (Rule 12/19 — never off-ledger, never a direct foreign write).
func (s *Service) Disburse(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID,
	kind string, amountMinor int64, currency string) (uuid.UUID, error) {
	if kind != "cash" && kind != "item" && kind != "service" && kind != "visit" {
		return uuid.Nil, ErrBadValue
	}
	if kind == "cash" && (amountMinor <= 0 || currency == "") {
		return uuid.Nil, ErrBadValue
	}
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return uuid.Nil, err
	}
	if c.State != "approved" {
		return uuid.Nil, ErrBadState
	}
	// claimant can't disburse their own grant — same SoD as decide.
	if fromPg(c.MembershipID) == oc.MembershipID {
		return uuid.Nil, ErrSelfAction
	}
	var bid uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		if pend, err := q.PendingBenefitForCase(ctx, c.ID); err == nil {
			// the disbursement can't exceed (or re-currency) the approved
			// grant — the pending row is what approvers signed off on.
			if kind == "cash" && pend.AmountMinor.Valid && amountMinor > pend.AmountMinor.Int64 {
				return ErrBadValue
			}
			if kind == "cash" && pend.Currency.Valid && pend.Currency.String != currency {
				return ErrBadValue
			}
			// fulfill the approve-time expense request
			n, err := q.MarkBenefitDisbursed(ctx, db.MarkBenefitDisbursedParams{
				ID: pend.ID, Kind: kind, AmountMinor: nInt(amountMinor), Currency: pgText(currency)})
			if err != nil {
				return err
			}
			if n == 0 {
				return ErrBadState // raced — already disbursed
			}
			bid = fromPg(pend.ID)
		} else {
			b, err := q.AddBenefit(ctx, db.AddBenefitParams{
				CaseID: c.ID, Kind: kind, AmountMinor: nInt(amountMinor), Currency: pgText(currency)})
			if err != nil {
				return err
			}
			bid = fromPg(b)
		}
		if n, err := q.SetWelfareCaseState(ctx, db.SetWelfareCaseStateParams{
			ID: c.ID, ToState: "disbursed", FromState: "approved"}); err != nil {
			return err
		} else if n == 0 {
			return ErrBadState // raced — another disbursal won the CAS
		}
		if err := s.auditCase(ctx, q, cid, oc.MembershipID, "disbursed", kind); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "BenefitDisbursed",
			"welfare_benefit", bid, oc.UserID, true,
			map[string]any{"benefit_id": bid, "case_id": cid,
				"member_id": fromPg(c.MembershipID), "kind": kind,
				"amount_minor": amountMinor, "currency": currency,
				"posted_by": oc.MembershipID})
	})
	return bid, err
}

// ---------- visits ----------

// ScheduleVisit — delegates are committee members visiting the member.
func (s *Service) ScheduleVisit(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID,
	at time.Time, delegates []uuid.UUID) (uuid.UUID, error) {
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return uuid.Nil, err
	}
	if c.State == "closed" || c.State == "rejected" {
		return uuid.Nil, ErrBadState
	}
	// every delegate must be a member of THIS org.
	for _, d := range delegates {
		m, err := s.q.GetMembershipByID(ctx, toPg(d))
		if err != nil || fromPg(m.OrganizationID) != oc.OrgID {
			return uuid.Nil, ErrNotFound
		}
	}
	var vid uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		v, err := q.CreateVisit(ctx, db.CreateVisitParams{
			CaseID: c.ID, ScheduledAt: pgtype.Timestamptz{Time: at, Valid: !at.IsZero()}})
		if err != nil {
			return err
		}
		vid = fromPg(v)
		for _, d := range delegates {
			if err := q.AddVisitDelegate(ctx, db.AddVisitDelegateParams{
				VisitID: v, MemberID: toPg(d)}); err != nil {
				return err
			}
		}
		if err := s.auditCase(ctx, q, cid, oc.MembershipID, "visit_scheduled", ""); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VisitScheduled",
			"welfare_visit", vid, oc.UserID, false,
			map[string]any{"visit_id": vid, "case_id": cid})
	})
	return vid, err
}

// CompleteVisit records the visit report — CAS on open visits only.
func (s *Service) CompleteVisit(ctx context.Context, oc *tenant.OrgCtx, cid, visitID uuid.UUID, report string) error {
	if _, err := s.GetCase(ctx, oc, cid); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.CompleteVisit(ctx, db.CompleteVisitParams{
			ID: toPg(visitID), Report: pgText(report), CaseID: toPg(cid)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound // unknown or already-completed visit
		}
		if err := s.auditCase(ctx, q, cid, oc.MembershipID, "visit_completed", ""); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VisitCompleted",
			"welfare_visit", visitID, oc.UserID, false,
			map[string]any{"visit_id": visitID, "case_id": cid})
	})
}

// ---------- close ----------

// CloseCase ends the case — from any live state.
func (s *Service) CloseCase(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, note string) error {
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return err
	}
	if c.State == "closed" {
		return nil // idempotent
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if n, err := q.SetWelfareCaseState(ctx, db.SetWelfareCaseStateParams{
			ID: c.ID, ToState: "closed", FromState: c.State}); err != nil {
			return err
		} else if n == 0 {
			return ErrBadState // raced — state moved under us
		}
		if err := s.auditCase(ctx, q, cid, oc.MembershipID, "closed", note); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CaseClosed",
			"welfare_case", cid, oc.UserID, true,
			map[string]any{"case_id": cid, "outcome": c.State})
	})
}

// ---------- policies ----------

// Policies surfaces the org.welfare.* rule namespace + case mix — the
// officers' view of the eligibility rules in force (Rule 23).
func (s *Service) Policies(ctx context.Context, oc *tenant.OrgCtx) (map[string]any, error) {
	rows, err := s.q.WelfareSettings(ctx, toPg(oc.OrgID))
	if err != nil {
		return nil, err
	}
	settings := map[string]json.RawMessage{}
	for _, r := range rows {
		settings[r.Key] = r.Value
	}
	mix, err := s.q.CaseCountByKind(ctx, toPg(oc.OrgID))
	if err != nil {
		return nil, err
	}
	return map[string]any{"settings": settings, "case_mix": mix}, nil
}

// ---------- MemberDeceased consumer ----------

// MemberDeceasedConsumer — §17: a death auto-suggests a bereavement case
// for the welfare chair (auto_suggested flag + CondolenceRegistered).
func (s *Service) MemberDeceasedConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "MemberDeceased" {
		return nil
	}
	midStr, _ := e.Payload["membership_id"].(string)
	mid, err := uuid.Parse(midStr)
	if err != nil || mid == uuid.Nil {
		return nil // payload contract violation — skip, don't retry forever
	}
	m, err := q.GetMembershipByID(ctx, toPg(mid))
	if err != nil {
		return nil // membership gone — nothing to suggest
	}
	orgID := fromPg(m.OrganizationID)
	tid, err := q.OrgTenantID(ctx, m.OrganizationID)
	if err != nil {
		return err
	}
	// at-least-once safety: one auto-suggested bereavement per member —
	// a re-delivered MemberDeceased must not spawn duplicates.
	if exists, err := q.AutoSuggestedCaseExists(ctx, m.ID); err != nil {
		return err
	} else if exists {
		return nil
	}
	cid, err := q.CreateWelfareCase(ctx, db.CreateWelfareCaseParams{
		OrgID: m.OrganizationID, MembershipID: m.ID, Kind: "bereavement",
		Title: "Bereavement (auto-suggested)", Details: pgText("MemberDeceased event"),
		AutoSuggested: true})
	if err != nil {
		return err
	}
	if err := s.auditCase(ctx, q, fromPg(cid), uuid.Nil, "auto_suggested", "member deceased"); err != nil {
		return err
	}
	return events.Emit(ctx, q, fromPg(tid), orgID, "CondolenceRegistered",
		"welfare_case", fromPg(cid), uuid.Nil, false,
		map[string]any{"case_id": fromPg(cid), "member_id": mid})
}
