// Ref: oga9-engineering/sections/16-governance.md — motions → resolutions,
// constitution versioning, committee mandates, handover. Term/appointment/
// position lifecycle lives in tenant/rbac.go (Phase 04 §03 tables).
package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrBadValue      = errors.New("bad value")
	ErrBadState      = errors.New("bad_motion_state")
	ErrQuorum        = errors.New("quorum_not_met")
	ErrBadResolution = errors.New("resolution_not_passed")
	ErrNotCommittee  = errors.New("not_a_committee")
)

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }
func pgInt(v int32) pgtype.Int4      { return pgtype.Int4{Int32: v, Valid: v != 0} }
func nInt64(v int64) pgtype.Int8     { return pgtype.Int8{Int64: v, Valid: v != 0} }

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// Cross-module seams wired in main (Rule 26 — service interfaces, never
	// foreign tables). quorumFn gates a motion's meeting before a vote can
	// record; archiveFn lands the handover snapshot in documents;
	// handoverSections maps label → producer (action items, custody,
	// approvals, finance snapshot) filled from other modules' services.
	quorumFn  func(ctx context.Context, oc *tenant.OrgCtx, activityID uuid.UUID) (bool, error)
	archiveFn func(ctx context.Context, oc *tenant.OrgCtx, title string, r io.Reader) error
	fineFn    func(ctx context.Context, orgID, memberID uuid.UUID, amount int64, currency, period string)
	// suspFn is the member-lifecycle seam for a 'suspension' sanction —
	// the members module owns lifecycle transitions (Rule 26).
	suspFn   func(ctx context.Context, oc *tenant.OrgCtx, memberID uuid.UUID) error
	sections map[string]func(ctx context.Context, oc *tenant.OrgCtx) (any, error)
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

func (s *Service) SetQuorum(fn func(context.Context, *tenant.OrgCtx, uuid.UUID) (bool, error)) {
	s.quorumFn = fn
}

func (s *Service) SetArchive(fn func(context.Context, *tenant.OrgCtx, string, io.Reader) error) {
	s.archiveFn = fn
}

// SetSuspension wires the members lifecycle seam for 'suspension'
// sanctions (tenant.Service.TransitionMember "suspend").
func (s *Service) SetSuspension(fn func(context.Context, *tenant.OrgCtx, uuid.UUID) error) {
	s.suspFn = fn
}

// SetHandoverSection registers a named report section (e.g. "action_items")
// fed by another module's service read.
func (s *Service) SetHandoverSection(name string, fn func(context.Context, *tenant.OrgCtx) (any, error)) {
	if s.sections == nil {
		s.sections = map[string]func(context.Context, *tenant.OrgCtx) (any, error){}
	}
	s.sections[name] = fn
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

// ---------- motions / resolutions ----------

// CreateMotion tables a motion — the mover is the acting member; the
// seconder (optional) must be a member of the same org.
func (s *Service) CreateMotion(ctx context.Context, oc *tenant.OrgCtx,
	activityID, seconderID uuid.UUID, title, body string) (uuid.UUID, error) {
	if title == "" || body == "" {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		if seconderID != uuid.Nil {
			m, err := q.GetMembershipByID(ctx, toPg(seconderID))
			if err != nil || fromPg(m.OrganizationID) != oc.OrgID {
				return ErrNotFound // seconder must belong to this org
			}
		}
		mid, err := q.CreateMotion(ctx, db.CreateMotionParams{
			OrgID: toPg(oc.OrgID), ActivityID: toPg(activityID),
			MoverID: toPg(oc.MembershipID), SeconderID: toPg(seconderID),
			Title: title, Body: body})
		if err != nil {
			return err
		}
		id = fromPg(mid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MotionTabled",
			"motion", id, oc.UserID, false,
			map[string]any{"motion_id": id, "org_id": oc.OrgID})
	})
	return id, err
}

func (s *Service) ListMotions(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.Motions, error) {
	return s.q.ListMotions(ctx, db.ListMotionsParams{OrgID: toPg(oc.OrgID), State: pgText(state)})
}

// RecordVote closes a motion into a resolution. §16: a motion tabled at a
// meeting requires its QuorumEvaluated result to be met before recording.
// The outcome lands as a term-scoped resolution (Rule 21).
func (s *Service) RecordVote(ctx context.Context, oc *tenant.OrgCtx, motionID uuid.UUID,
	outcome string, vFor, vAgainst, vAbstain int32) (uuid.UUID, error) {
	if outcome != "passed" && outcome != "rejected" && outcome != "amended_passed" {
		return uuid.Nil, ErrBadValue
	}
	m, err := s.q.GetMotion(ctx, db.GetMotionParams{ID: toPg(motionID), OrgID: toPg(oc.OrgID)})
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	if m.State != "tabled" && m.State != "debated" {
		return uuid.Nil, ErrBadState
	}
	if m.ActivityID.Valid && s.quorumFn != nil {
		met, err := s.quorumFn(ctx, oc, fromPg(m.ActivityID))
		if err != nil {
			return uuid.Nil, err
		}
		if !met {
			return uuid.Nil, ErrQuorum
		}
	}
	termID, err := s.q.CurrentTermID(ctx, toPg(oc.OrgID))
	if err != nil {
		return uuid.Nil, ErrBadValue // resolutions bind to the current term (Rule 21)
	}
	var rid uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetMotionState(ctx, db.SetMotionStateParams{
			ID: m.ID, ToState: "voted", FromState: m.State})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState // raced — another recorder won the CAS
		}
		rr, err := q.CreateResolution(ctx, db.CreateResolutionParams{
			OrgID: toPg(oc.OrgID), MotionID: m.ID, ActivityID: m.ActivityID,
			TermID: termID, Title: m.Title, Body: m.Body, Outcome: outcome,
			VotesFor: pgInt(vFor), VotesAgainst: pgInt(vAgainst), VotesAbstain: pgInt(vAbstain)})
		if err != nil {
			return err
		}
		rid = fromPg(rr)
		ev := "ResolutionRejected"
		if outcome != "rejected" {
			ev = "ResolutionPassed"
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev, "resolution", rid,
			oc.UserID, true,
			map[string]any{"resolution_id": rid, "motion_id": motionID,
				"meeting_id": fromPg(m.ActivityID), "outcome": outcome})
	})
	return rid, err
}

func (s *Service) ListResolutions(ctx context.Context, oc *tenant.OrgCtx) ([]db.Resolutions, error) {
	return s.q.ListResolutions(ctx, toPg(oc.OrgID))
}

// ---------- constitution ----------

func (s *Service) ListConstitution(ctx context.Context, oc *tenant.OrgCtx) ([]db.ConstitutionVersions, error) {
	return s.q.ListConstitution(ctx, toPg(oc.OrgID))
}

func (s *Service) CurrentConstitution(ctx context.Context, oc *tenant.OrgCtx) (db.ConstitutionVersions, error) {
	v, err := s.q.CurrentConstitution(ctx, toPg(oc.OrgID))
	if err != nil {
		return v, ErrNotFound
	}
	return v, nil
}

// AdoptConstitution appends the next version — §16 requires a passed
// resolution reference. v1 emits ConstitutionAdopted; later versions emit
// ConstitutionAmended. Prior versions stay readable.
func (s *Service) AdoptConstitution(ctx context.Context, oc *tenant.OrgCtx, body string, resolutionID uuid.UUID) (uuid.UUID, int32, error) {
	if body == "" || resolutionID == uuid.Nil {
		return uuid.Nil, 0, ErrBadValue
	}
	r, err := s.q.GetResolution(ctx, db.GetResolutionParams{ID: toPg(resolutionID), OrgID: toPg(oc.OrgID)})
	if err != nil {
		return uuid.Nil, 0, ErrNotFound
	}
	if r.Outcome != "passed" && r.Outcome != "amended_passed" {
		return uuid.Nil, 0, ErrBadResolution
	}
	// A resolution ratifies ONE constitution version — reusing it would
	// let the same vote mint unlimited versions.
	if vs, err := s.q.ListConstitution(ctx, toPg(oc.OrgID)); err == nil {
		for _, v := range vs {
			if fromPg(v.AdoptedByResolutionID) == resolutionID {
				return uuid.Nil, 0, ErrBadResolution
			}
		}
	}
	var id uuid.UUID
	var version int32
	err = s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.AdoptConstitution(ctx, db.AdoptConstitutionParams{
			OrgID: toPg(oc.OrgID), Body: body, ResolutionID: toPg(resolutionID)})
		if err != nil {
			return err
		}
		id, version = fromPg(row.ID), row.Version
		ev := "ConstitutionAdopted"
		if version > 1 {
			ev = "ConstitutionAmended"
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev,
			"constitution_version", id, oc.UserID, true,
			map[string]any{"constitution_version_id": id, "version": version,
				"resolution_id": resolutionID, "org_id": oc.OrgID})
	})
	return id, version, err
}

// ---------- committee mandates ----------

func (s *Service) GetMandate(ctx context.Context, oc *tenant.OrgCtx, unitID uuid.UUID) (db.CommitteeMandates, error) {
	// org-scoped: a foreign-org unit id must not resolve a mandate.
	m, err := s.q.GetMandateScoped(ctx, db.GetMandateScopedParams{
		UnitID: toPg(unitID), OrgID: toPg(oc.OrgID)})
	if err != nil {
		return m, ErrNotFound
	}
	return m, nil
}

// ListMandates returns the org's active committee mandates.
func (s *Service) ListMandates(ctx context.Context, oc *tenant.OrgCtx) ([]db.CommitteeMandates, error) {
	return s.q.ListCommitteeMandates(ctx, toPg(oc.OrgID))
}

// UpsertMandate defines a committee's charter — §16: mandates apply to
// org_units of type 'committee' only. budget_limit_minor is enforced on
// linked expense approvals via MandateLimit.
func (s *Service) UpsertMandate(ctx context.Context, oc *tenant.OrgCtx, unitID uuid.UUID,
	mandate string, reportsTo uuid.UUID, limitMinor int64, currency string, active bool) error {
	if mandate == "" {
		return ErrBadValue
	}
	t, err := s.q.GetCommitteeUnit(ctx, db.GetCommitteeUnitParams{
		ID: toPg(unitID), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if t != "committee" {
		return ErrNotCommittee
	}
	return s.q.UpsertMandate(ctx, db.UpsertMandateParams{
		UnitID: toPg(unitID), Mandate: mandate, ReportsTo: toPg(reportsTo),
		BudgetLimitMinor: nInt64(limitMinor), Currency: pgText(currency), Active: active})
}

// MandateLimit is the finance seam for §16's "committee spend over mandate
// limit → approval blocked": returns the active cap for a committee unit.
// Expenses gain the committee link with their owning section; the check
// lands there via this interface (Rule 26).
func (s *Service) MandateLimit(ctx context.Context, unitID uuid.UUID) (int64, string, bool) {
	m, err := s.q.GetMandate(ctx, toPg(unitID))
	if err != nil || !m.Active || !m.BudgetLimitMinor.Valid {
		return 0, "", false
	}
	return m.BudgetLimitMinor.Int64, m.Currency.String, true
}

// ---------- handover ----------

// Handover composes the §16 report — open action items + custody assets +
// pending approvals + finances snapshot, each produced by the owning
// module's service (SetHandoverSection seams). Sections absent from the
// wiring come back as empty objects, not fabrications.
func (s *Service) Handover(ctx context.Context, oc *tenant.OrgCtx) (map[string]any, error) {
	report := map[string]any{
		"generated_at": time.Now().UTC(), "org_id": oc.OrgID, "sections": map[string]any{},
	}
	secs := report["sections"].(map[string]any)
	for name, fn := range s.sections {
		v, err := fn(ctx, oc)
		if err != nil {
			secs[name] = map[string]any{"error": err.Error()} // degrade, don't drop the report
			continue
		}
		secs[name] = v
	}
	return report, nil
}

// ArchiveHandover generates the report, stores it in documents (kind
// 'handover'), and emits HandoverGenerated — deduped per term so the
// reminders sweep and term-close both produce exactly one row.
func (s *Service) ArchiveHandover(ctx context.Context, oc *tenant.OrgCtx, termID uuid.UUID) error {
	exists, err := s.q.HandoverGeneratedExists(ctx, toPg(termID))
	if err != nil || exists {
		return err
	}
	report, err := s.Handover(ctx, oc)
	if err != nil {
		return err
	}
	if s.archiveFn != nil {
		if err := s.archiveFn(ctx, oc, fmt.Sprintf("handover-term-%s", termID),
			strings.NewReader(string(mustJSON(report)))); err != nil {
			return err
		}
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		// re-check inside the tx: the dedupe read and the marker write
		// must be one atomic step or a concurrent path double-archives.
		exists, err := q.HandoverGeneratedExists(ctx, toPg(termID))
		if err != nil || exists {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "HandoverGenerated",
			"term", termID, oc.UserID, false,
			map[string]any{"term_id": termID, "org_id": oc.OrgID})
	})
}

// ---------- sweep feeds ----------

// ExpiredTerms / TermsEndingSoon feed governance:term_transitions and
// governance:handover_reminders.
func (s *Service) ExpiredTerms(ctx context.Context) ([]db.Terms, error) {
	return s.q.ExpiredCurrentTerms(ctx)
}

func (s *Service) TermsEndingSoon(ctx context.Context, days int32) ([]db.Terms, error) {
	return s.q.TermEndingSoon(ctx, days)
}

// OrgTenant resolves the event tenant for system-scoped runs (no OrgCtx).
func (s *Service) OrgTenant(ctx context.Context, orgID uuid.UUID) (uuid.UUID, error) {
	t, err := s.q.OrgTenantID(ctx, toPg(orgID))
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(t), nil
}

// TermEndHandoverConsumer — §16: handover generates at term close.
// TermEnded (manual close or the transitions sweep) triggers the
// archive; ArchiveHandover dedupes per term, so the reminders sweep and
// this path can never double-produce.
func (s *Service) TermEndHandoverConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "TermEnded" {
		return nil
	}
	termStr, _ := e.Payload["term_id"].(string)
	termID, err := uuid.Parse(termStr)
	if err != nil || termID == uuid.Nil {
		return nil
	}
	tid, err := q.OrgTenantID(ctx, toPg(e.OrganizationID))
	if err != nil {
		return err
	}
	return s.ArchiveHandover(ctx, &tenant.OrgCtx{OrgID: e.OrganizationID, TenantID: fromPg(tid)}, termID)
}

// CloseExpiredTermData returns the rows the transitions sweep needs —
// the actual close (revoke grants, events, cache invalidation) lives in
// the tenant service's term lifecycle.
func mustJSON(v any) []byte {
	b, _ := json.MarshalIndent(v, "", "  ")
	return b
}

// DebateMotion — tabled → debated (§16 state machine; the recorder marks
// the motion as under active debate before a vote is called).
func (s *Service) DebateMotion(ctx context.Context, oc *tenant.OrgCtx, motionID uuid.UUID) error {
	m, err := s.q.GetMotion(ctx, db.GetMotionParams{ID: toPg(motionID), OrgID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if m.State != "tabled" {
		return ErrBadState
	}
	n, err := s.q.SetMotionState(ctx, db.SetMotionStateParams{
		ID: m.ID, ToState: "debated", FromState: "tabled"})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBadState
	}
	return nil
}

// WithdrawMotion — tabled|debated → withdrawn (the mover/sponsor pulls it
// before a vote). §16 state machine includes 'withdrawn'.
func (s *Service) WithdrawMotion(ctx context.Context, oc *tenant.OrgCtx, motionID uuid.UUID) error {
	m, err := s.q.GetMotion(ctx, db.GetMotionParams{ID: toPg(motionID), OrgID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if m.State != "tabled" && m.State != "debated" {
		return ErrBadState
	}
	n, err := s.q.SetMotionState(ctx, db.SetMotionStateParams{
		ID: m.ID, ToState: "withdrawn", FromState: m.State})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBadState
	}
	return nil
}

// CompleteHandover — §16: the incoming EXCO acknowledges the handover
// report; emits HandoverCompleted for the supplied term (idempotent —
// completing twice is a no-op via the generated-row check).
func (s *Service) CompleteHandover(ctx context.Context, oc *tenant.OrgCtx, termID uuid.UUID) error {
	done, err := s.q.HandoverCompletedExists(ctx, toPg(termID))
	if err != nil || done {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		done, err := q.HandoverCompletedExists(ctx, toPg(termID))
		if err != nil || done {
			return err // atomic with the marker below
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "HandoverCompleted",
			"term", termID, oc.UserID, false,
			map[string]any{"term_id": termID, "org_id": oc.OrgID})
	})
}
