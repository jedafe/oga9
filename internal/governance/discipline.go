// Ref: oga9-engineering/sections/16-governance.md — discipline & grievance
// casework: open → investigating → hearing → resolved → (appeal →
// upheld|overturned) → closed. Fine sanctions post through §12 via the
// finance seam — never off-ledger (Rule 19/26).
package governance

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNoHearing  = errors.New("hearing_required")
	ErrBadAppeal  = errors.New("appeal_not_allowed")
	ErrNotSubject = errors.New("not_the_subject")
)

var disciplineKinds = map[string]bool{
	"conduct": true, "financial": true, "attendance": true, "other": true}
var sanctions = map[string]bool{
	"none": true, "warning": true, "fine": true, "suspension": true}

// SetFine wires the §12 fine-assessment seam (finance.Service.AssessFine):
// a fine sanction lands as a 'fine' contribution-type obligation for the
// subject — period label 'discipline:{case_id}' dedupes re-resolution.
func (s *Service) SetFine(fn func(ctx context.Context, orgID, memberID uuid.UUID, amount int64, currency, period string)) {
	s.fineFn = fn
}

// OpenCase — officer files a case against a member of THIS org (subject
// verified against the org, never request-trusted).
func (s *Service) OpenCase(ctx context.Context, oc *tenant.OrgCtx,
	subjectID uuid.UUID, kind, title, details string) (uuid.UUID, error) {
	if !disciplineKinds[kind] || title == "" {
		return uuid.Nil, ErrBadValue
	}
	m, err := s.q.GetMembershipByID(ctx, toPg(subjectID))
	if err != nil || fromPg(m.OrganizationID) != oc.OrgID {
		return uuid.Nil, ErrNotFound
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		cid, err := q.CreateDisciplineCase(ctx, db.CreateDisciplineCaseParams{
			OrganizationID: toPg(oc.OrgID), SubjectID: toPg(subjectID),
			ReportedBy: toPg(oc.MembershipID), Kind: kind, Title: title,
			Details: pgText(details)})
		if err != nil {
			return err
		}
		id = fromPg(cid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DisciplineCaseOpened",
			"discipline_case", id, oc.UserID, true,
			map[string]any{"case_id": id, "subject_id": subjectID, "kind": kind})
	})
	return id, err
}

func (s *Service) ListCases(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.DisciplineCases, error) {
	return s.q.ListDisciplineCases(ctx, db.ListDisciplineCasesParams{
		OrganizationID: toPg(oc.OrgID), Column2: state})
}

func (s *Service) GetCase(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID) (db.DisciplineCases, error) {
	c, err := s.q.GetDisciplineCase(ctx, db.GetDisciplineCaseParams{
		ID: toPg(cid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return c, ErrNotFound
	}
	return c, nil
}

// MyCases — the subject-facing view (/me/discipline).
func (s *Service) MyCases(ctx context.Context, userID uuid.UUID) ([]db.DisciplineCases, error) {
	return s.q.MyDisciplineCases(ctx, toPg(userID))
}

// Advance — PATCH transitions: open → investigating; any live state →
// dismissed. 'hearing' is entered via ScheduleHearing.
func (s *Service) Advance(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID, action string) error {
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return err
	}
	var to string
	switch action {
	case "investigate":
		if c.State != "open" {
			return ErrBadState
		}
		to = "investigating"
	case "dismiss":
		switch c.State {
		case "open", "investigating", "hearing":
			to = "dismissed"
		default:
			return ErrBadState
		}
	case "reopen":
		if c.State != "dismissed" {
			return ErrBadState
		}
		to = "open"
	default:
		return ErrBadValue
	}
	n, err := s.q.SetDisciplineCaseState(ctx, db.SetDisciplineCaseStateParams{
		ID: c.ID, State: to, State_2: c.State})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBadState
	}
	return nil
}

// ScheduleHearing — investigating → hearing; a case needs at least one
// held hearing before it can resolve (natural-justice rule).
func (s *Service) ScheduleHearing(ctx context.Context, oc *tenant.OrgCtx,
	cid uuid.UUID, at time.Time) (uuid.UUID, error) {
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return uuid.Nil, err
	}
	if c.State != "investigating" && c.State != "hearing" {
		return uuid.Nil, ErrBadState
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		hid, err := q.CreateHearing(ctx, db.CreateHearingParams{
			CaseID: c.ID, ScheduledAt: pgtype.Timestamptz{Time: at, Valid: !at.IsZero()}})
		if err != nil {
			return err
		}
		if n, err := q.SetDisciplineCaseState(ctx, db.SetDisciplineCaseStateParams{
			ID: c.ID, State: "hearing", State_2: c.State}); err != nil {
			return err
		} else if n == 0 {
			return ErrBadState
		}
		id = fromPg(hid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DisciplineHearingScheduled",
			"discipline_case", cid, oc.UserID, true,
			map[string]any{"case_id": cid, "hearing_id": id})
	})
	return id, err
}

func (s *Service) ListHearings(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID) ([]db.DisciplineHearings, error) {
	if _, err := s.GetCase(ctx, oc, cid); err != nil {
		return nil, err
	}
	return s.q.ListHearings(ctx, toPg(cid))
}

// CompleteHearing records the held hearing's outcome.
func (s *Service) CompleteHearing(ctx context.Context, oc *tenant.OrgCtx,
	cid, hid uuid.UUID, outcome, notes string) error {
	if _, err := s.GetCase(ctx, oc, cid); err != nil {
		return err
	}
	n, err := s.q.CompleteHearing(ctx, db.CompleteHearingParams{
		ID: toPg(hid), Outcome: pgText(outcome), Notes: pgText(notes), CaseID: toPg(cid)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Resolve closes the casework with a sanction — requires at least one
// held hearing (§16 flow). A fine posts to §12 through the fine seam.
func (s *Service) Resolve(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID,
	sanction string, fineMinor int64, currency string) error {
	if !sanctions[sanction] {
		return ErrBadValue
	}
	if sanction == "fine" && (fineMinor <= 0 || currency == "") {
		return ErrBadValue
	}
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return err
	}
	hearings, err := s.q.ListHearings(ctx, c.ID)
	if err != nil {
		return err
	}
	held := false
	for _, h := range hearings {
		if h.HeldAt.Valid {
			held = true
			break
		}
	}
	if !held {
		return ErrNoHearing
	}
	// Rule 12: state flip + event append commit together.
	if err := s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.ResolveDisciplineCase(ctx, db.ResolveDisciplineCaseParams{
			ID: c.ID, Sanction: pgText(sanction), FineMinor: nInt64(fineMinor), Currency: pgText(currency)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DisciplineResolved",
			"discipline_case", cid, oc.UserID, true,
			map[string]any{"case_id": cid, "subject_id": fromPg(c.SubjectID),
				"sanction": sanction, "fine_minor": fineMinor})
	}); err != nil {
		return err
	}
	// §12 link — the fine posts through the finance seam after the case
	// commits (assessFine dedupes on the discipline:{id} period label and
	// logs its own failures loudly).
	if sanction == "fine" && s.fineFn != nil {
		s.fineFn(ctx, oc.OrgID, fromPg(c.SubjectID), fineMinor, currency, "discipline:"+cid.String())
	}
	// a 'suspension' sanction must actually suspend — the members seam
	// runs the lifecycle transition (active → suspended), which revokes
	// cached org context + permission caches on its own side (Rule 6/26).
	if sanction == "suspension" && s.suspFn != nil {
		if err := s.suspFn(ctx, oc, fromPg(c.SubjectID)); err != nil {
			return fmt.Errorf("sanction recorded but suspension failed: %w", err)
		}
	}
	return nil
}

// FileAppeal — the SUBJECT appeals a resolved case (org.discipline.appeal_own
// checked at the route; ownership re-verified here — Rule 24 pattern).
func (s *Service) FileAppeal(ctx context.Context, oc *tenant.OrgCtx,
	cid uuid.UUID, grounds string) (uuid.UUID, error) {
	if grounds == "" {
		return uuid.Nil, ErrBadValue
	}
	c, err := s.GetCase(ctx, oc, cid)
	if err != nil {
		return uuid.Nil, err
	}
	if fromPg(c.SubjectID) != oc.MembershipID {
		return uuid.Nil, ErrNotSubject // only the subject may appeal
	}
	if c.State != "resolved" {
		return uuid.Nil, ErrBadAppeal
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		aid, err := q.FileAppeal(ctx, db.FileAppealParams{
			CaseID: c.ID, FiledBy: toPg(oc.MembershipID), Grounds: grounds})
		if err != nil {
			return err
		}
		if n, err := q.SetDisciplineCaseState(ctx, db.SetDisciplineCaseStateParams{
			ID: c.ID, State: "appealed", State_2: "resolved"}); err != nil {
			return err
		} else if n == 0 {
			return ErrBadAppeal // raced — another appeal/outcome won
		}
		id = fromPg(aid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DisciplineAppealFiled",
			"discipline_case", cid, oc.UserID, true,
			map[string]any{"case_id": cid, "appeal_id": id})
	})
	return id, err
}

func (s *Service) ListAppeals(ctx context.Context, oc *tenant.OrgCtx, cid uuid.UUID) ([]db.DisciplineAppeals, error) {
	if _, err := s.GetCase(ctx, oc, cid); err != nil {
		return nil, err
	}
	return s.q.ListAppeals(ctx, toPg(cid))
}

// DecideAppeal — upheld: case closes with the sanction standing;
// overturned: case closes, sanction voided (a fine already posted is a
// finance-side waiver decision, not a silent ledger edit — Rule 19).
func (s *Service) DecideAppeal(ctx context.Context, oc *tenant.OrgCtx,
	aid uuid.UUID, uphold bool) error {
	a, err := s.q.GetAppeal(ctx, db.GetAppealParams{ID: toPg(aid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	decision := "overturned"
	if uphold {
		decision = "upheld"
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.DecideAppeal(ctx, db.DecideAppealParams{ID: a.ID, State: decision})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState // already decided
		}
		if _, err := q.SetDisciplineCaseState(ctx, db.SetDisciplineCaseStateParams{
			ID: a.CaseID, State: "closed", State_2: "appealed"}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DisciplineAppealDecided",
			"discipline_case", fromPg(a.CaseID), oc.UserID, true,
			map[string]any{"case_id": fromPg(a.CaseID), "appeal_id": aid, "decision": decision})
	})
}
