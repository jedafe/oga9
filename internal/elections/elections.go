// Ref: oga9-engineering/sections/21-elections.md — secrecy is structural:
// ballots prove participation (one per member); votes are anonymous rows
// with no correlatable field. Eligibility is computed, never stored.
package elections

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrBadValue     = errors.New("bad value")
	ErrBadState     = errors.New("bad_election_state")
	ErrNotEligible  = errors.New("not_eligible")
	ErrAlreadyVoted = errors.New("already_voted")
	ErrDuplicate    = errors.New("already_nominated")
)

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }

// eligibilityRules decode the election's JSONB — all rules resolve at
// check time (§21: computed, never stored; Rule 23).
type eligibilityRules struct {
	MinTenureDays        int64    `json:"min_tenure_days"`
	Categories           []string `json:"categories"`
	RequiresGoodStanding bool     `json:"requires_good_standing"`
}

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// appointFn seats winners into officer_appointments at ratify time —
	// governance stays the writer of its own tables (Rule 26 seam).
	// branch/committee carry the winning candidate's unit scope (§21).
	appointFn func(ctx context.Context, oc *tenant.OrgCtx, termID, positionID,
		memberID, branchID, committeeID uuid.UUID, startsAt time.Time) (uuid.UUID, error)
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

func (s *Service) SetAppoint(fn func(context.Context, *tenant.OrgCtx, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, time.Time) (uuid.UUID, error)) {
	s.appointFn = fn
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

// audit — §21: every nomination/vet/vote/ratify writes audit_events.
func (s *Service) audit(ctx context.Context, q *db.Queries, oc *tenant.OrgCtx,
	electionID uuid.UUID, action string, extra map[string]any) error {
	return s.auditRow(ctx, q, oc.TenantID, oc.OrgID, oc.UserID, electionID, action, extra)
}

// auditRow — the sweep variant: system transitions carry a null actor
// (audit_events.actor_id is nullable; the sweep is not a user).
func (s *Service) auditRow(ctx context.Context, q *db.Queries, tenantID, orgID, actorID, electionID uuid.UUID,
	action string, extra map[string]any) error {
	raw, _ := json.Marshal(extra)
	return q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		TenantID: toPg(tenantID), OrganizationID: toPg(orgID),
		ActorID: toPg(actorID), Action: action, TargetType: pgText("election"),
		TargetID: toPg(electionID), Context: raw})
}

// eligibility computes the member's standing against the election's
// rules: active + category + tenure + good standing.
func (s *Service) eligibility(ctx context.Context, e db.Elections, memberID uuid.UUID) error {
	m, err := s.q.GetMembershipByID(ctx, toPg(memberID))
	if err != nil {
		return ErrNotFound
	}
	// isolation: the membership must belong to THIS election's org —
	// without this a foreign-org member could be nominated.
	if m.OrganizationID != e.OrganizationID {
		return ErrNotEligible
	}
	if m.LifecycleState != "active" {
		return ErrNotEligible
	}
	var rules eligibilityRules
	if err := json.Unmarshal(e.Eligibility, &rules); err != nil {
		return ErrNotEligible // malformed rules fail closed
	}
	if len(rules.Categories) > 0 {
		ok := false
		for _, c := range rules.Categories {
			if c == m.Category {
				ok = true
				break
			}
		}
		if !ok {
			return ErrNotEligible
		}
	}
	if rules.MinTenureDays > 0 {
		since := m.CreatedAt.Time
		if m.JoinedAt.Valid {
			since = m.JoinedAt.Time
		}
		if time.Since(since) < time.Duration(rules.MinTenureDays)*24*time.Hour {
			return ErrNotEligible
		}
	}
	if rules.RequiresGoodStanding && m.DuesStanding != "good" {
		return ErrNotEligible
	}
	return nil
}

// ---------- lifecycle ----------

// Create — an election elects officers for the NEXT term (term_id).
func (s *Service) Create(ctx context.Context, oc *tenant.OrgCtx, termID uuid.UUID,
	title string, eligibility json.RawMessage) (uuid.UUID, error) {
	if title == "" || termID == uuid.Nil {
		return uuid.Nil, ErrBadValue
	}
	// the term must belong to this org — electing into a foreign term
	// would graft another org's seats.
	if tr, err := s.q.GetTermOrg(ctx, toPg(termID)); err != nil || fromPg(tr.OrganizationID) != oc.OrgID {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		eid, err := q.CreateElection(ctx, db.CreateElectionParams{
			OrgID: toPg(oc.OrgID), TermID: toPg(termID), Title: title, Eligibility: eligibility})
		if err != nil {
			return err
		}
		id = fromPg(eid)
		if err := s.audit(ctx, q, oc, id, "election_created", nil); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ElectionStarted",
			"election", id, oc.UserID, true,
			map[string]any{"election_id": id, "term_id": termID})
	})
	return id, err
}

func (s *Service) List(ctx context.Context, oc *tenant.OrgCtx) ([]db.Elections, error) {
	return s.q.ListElections(ctx, toPg(oc.OrgID))
}

func (s *Service) Get(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) (db.Elections, error) {
	e, err := s.q.GetElection(ctx, db.GetElectionParams{ID: toPg(eid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return e, ErrNotFound
	}
	return e, nil
}

// Schedule — PATCH: set the nomination/voting windows (pre-voting only).
func (s *Service) Schedule(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID,
	nominationsOpen, votingOpens, votingCloses time.Time) error {
	if _, err := s.Get(ctx, oc, eid); err != nil {
		return err
	}
	if !votingOpens.IsZero() && !votingCloses.IsZero() && !votingOpens.Before(votingCloses) {
		return ErrBadValue // voting window must open before it closes
	}
	if !nominationsOpen.IsZero() && !votingOpens.IsZero() && !nominationsOpen.Before(votingOpens) {
		return ErrBadValue // nominations must precede voting
	}
	n, err := s.q.ScheduleElection(ctx, db.ScheduleElectionParams{
		ID:              toPg(eid),
		NominationsOpen: pgtype.Timestamptz{Time: nominationsOpen, Valid: !nominationsOpen.IsZero()},
		VotingOpens:     pgtype.Timestamptz{Time: votingOpens, Valid: !votingOpens.IsZero()},
		VotingCloses:    pgtype.Timestamptz{Time: votingCloses, Valid: !votingCloses.IsZero()}})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBadState
	}
	return nil
}

// transition moves the election one step; the sweeps drive the same
// transitions on schedule.
func (s *Service) transition(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID,
	from, to, ev, auditAction string) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetElectionState(ctx, db.SetElectionStateParams{
			ID: toPg(eid), State: to, State_2: from})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState
		}
		if err := s.audit(ctx, q, oc, eid, auditAction, map[string]any{"state": to}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev,
			"election", eid, oc.UserID, false,
			map[string]any{"election_id": eid, "state": to})
	})
}

func (s *Service) BeginNominations(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) error {
	return s.transition(ctx, oc, eid, "setup", "nominations", "NominationsOpened", "nominations_opened")
}

func (s *Service) OpenVoting(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) error {
	e, err := s.Get(ctx, oc, eid)
	if err != nil {
		return err
	}
	if e.State != "nominations" && e.State != "campaign" {
		return ErrBadState
	}
	return s.transition(ctx, oc, eid, e.State, "voting", "VotingOpened", "voting_opened")
}

// CloseVoting — voting → concluded; the tally is computable from the
// anonymous votes table from this point.
func (s *Service) CloseVoting(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) error {
	if err := s.transition(ctx, oc, eid, "voting", "concluded", "VotingClosed", "voting_closed"); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ElectionConcluded",
			"election", eid, oc.UserID, true,
			map[string]any{"election_id": eid})
	})
}

// ---------- nominations ----------

// Nominate — nominee must clear the election's eligibility rules.
func (s *Service) Nominate(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID,
	positionID, memberID, scopeUnitID uuid.UUID, manifesto string) (uuid.UUID, error) {
	e, err := s.Get(ctx, oc, eid)
	if err != nil {
		return uuid.Nil, err
	}
	if e.State != "nominations" && e.State != "campaign" {
		return uuid.Nil, ErrBadState
	}
	positions, err := s.q.OrgPositions(ctx, e.OrganizationID)
	if err != nil {
		return uuid.Nil, err
	}
	valid := false
	for _, p := range positions {
		if fromPg(p.ID) == positionID {
			valid = true
			break
		}
	}
	if !valid {
		return uuid.Nil, ErrBadValue // foreign/unknown seat
	}
	if err := s.eligibility(ctx, e, memberID); err != nil {
		return uuid.Nil, err
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		cid, err := q.NominateCandidate(ctx, db.NominateCandidateParams{
			ElectionID: e.ID, PositionID: toPg(positionID), MembershipID: toPg(memberID),
			ScopeUnitID: toPg(scopeUnitID),
			Manifesto:   pgText(manifesto), NominatedBy: toPg(oc.MembershipID)})
		if err != nil {
			if isUniqueViolation(err) {
				return ErrDuplicate
			}
			return err
		}
		id = fromPg(cid)
		if err := s.audit(ctx, q, oc, eid, "candidate_nominated",
			map[string]any{"candidate_id": id, "position_id": positionID, "member_id": memberID}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CandidateNominated",
			"candidate", id, oc.UserID, true,
			map[string]any{"candidate_id": id, "election_id": eid, "position_id": positionID})
	})
	return id, err
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func (s *Service) ListCandidates(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) ([]db.ListCandidatesRow, error) {
	if _, err := s.Get(ctx, oc, eid); err != nil {
		return nil, err
	}
	return s.q.ListCandidates(ctx, toPg(eid))
}

// Vet — nominated → vetted|disqualified (withdraw rides the same edge).
func (s *Service) Vet(ctx context.Context, oc *tenant.OrgCtx, eid, cid uuid.UUID, decision, reason string) error {
	if decision != "vetted" && decision != "disqualified" && decision != "withdrawn" {
		return ErrBadValue
	}
	if _, err := s.Get(ctx, oc, eid); err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetCandidateState(ctx, db.SetCandidateStateParams{
			ID: toPg(cid), ElectionID: toPg(eid), State: decision})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound // unknown candidate or already decided
		}
		if err := s.audit(ctx, q, oc, eid, "candidate_"+decision,
			map[string]any{"candidate_id": cid, "reason": reason}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CandidateVetted",
			"candidate", cid, oc.UserID, false,
			map[string]any{"candidate_id": cid, "election_id": eid, "decision": decision})
	})
}

// ---------- ballot + vote ----------

// Ballot — the member's view: org positions, vetted candidates, their
// eligibility + whether they've already cast.
func (s *Service) Ballot(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) (map[string]any, error) {
	e, err := s.Get(ctx, oc, eid)
	if err != nil {
		return nil, err
	}
	positions, err := s.q.OrgPositions(ctx, toPg(oc.OrgID))
	if err != nil {
		return nil, err
	}
	candsAll, err := s.q.ListCandidates(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	var candidates []db.ListCandidatesRow
	for _, c := range candsAll {
		if c.State == "vetted" {
			candidates = append(candidates, c)
		}
	}
	voted, err := s.q.BallotExists(ctx, db.BallotExistsParams{
		ElectionID: e.ID, MembershipID: toPg(oc.MembershipID)})
	if err != nil {
		return nil, err
	}
	eligible := s.eligibility(ctx, e, oc.MembershipID) == nil
	return map[string]any{"election": e, "positions": positions,
		"candidates": candidates, "already_voted": voted, "eligible": eligible}, nil
}

type VoteChoice struct {
	PositionID  uuid.UUID `json:"position_id"`
	CandidateID uuid.UUID `json:"candidate_id"` // nil = abstain that seat
}

// Vote — one tx: participation receipt (ballots) + anonymous choice rows
// (votes). The election row lock serializes cast_seq. Eligibility is
// checked at cast; second attempts hit the ballot UNIQUE → already_voted.
func (s *Service) Vote(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID, choices []VoteChoice) error {
	e, err := s.Get(ctx, oc, eid)
	if err != nil {
		return err
	}
	if e.State != "voting" {
		return ErrBadState
	}
	if err := s.eligibility(ctx, e, oc.MembershipID); err != nil {
		return err
	}
	if len(choices) == 0 {
		return ErrBadValue
	}
	// validate every choice before opening the tx — candidates must be
	// vetted, belong to this election + position (or nil = abstain).
	positions, err := s.q.OrgPositions(ctx, e.OrganizationID)
	if err != nil {
		return err
	}
	orgPositions := map[uuid.UUID]bool{}
	for _, p := range positions {
		orgPositions[fromPg(p.ID)] = true
	}
	cands, err := s.q.ListCandidates(ctx, e.ID)
	if err != nil {
		return err
	}
	ok := map[uuid.UUID]uuid.UUID{} // candidate → position
	for _, c := range cands {
		if c.State == "vetted" {
			ok[fromPg(c.ID)] = fromPg(c.PositionID)
		}
	}
	seenPositions := map[uuid.UUID]bool{}
	for _, ch := range choices {
		if !orgPositions[ch.PositionID] {
			return ErrBadValue // seat isn't in this org's position list
		}
		if ch.CandidateID != uuid.Nil {
			pos, found := ok[ch.CandidateID]
			if !found || pos != ch.PositionID {
				return ErrBadValue // not a vetted candidate for that seat
			}
		}
		if seenPositions[ch.PositionID] {
			return ErrBadValue // one choice per seat
		}
		seenPositions[ch.PositionID] = true
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		locked, err := q.GetElectionForUpdate(ctx, e.ID)
		if err != nil {
			return err
		}
		if locked.State != "voting" {
			return ErrBadState // voting closed between the check and the lock
		}
		if _, err := q.InsertBallot(ctx, db.InsertBallotParams{
			ElectionID: e.ID, MembershipID: toPg(oc.MembershipID)}); err != nil {
			if isUniqueViolation(err) {
				return ErrAlreadyVoted
			}
			return err
		}
		seq, err := q.NextCastSeq(ctx, e.ID)
		if err != nil {
			return err
		}
		for _, ch := range choices {
			if err := q.InsertVote(ctx, db.InsertVoteParams{
				ElectionID: e.ID, PositionID: toPg(ch.PositionID),
				CandidateID: toPg(ch.CandidateID), CastSeq: int64(seq)}); err != nil {
				return err
			}
		}
		if err := s.audit(ctx, q, oc, eid, "ballot_cast",
			map[string]any{"member_id": oc.MembershipID}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "BallotCast",
			"ballot", eid, oc.UserID, true,
			map[string]any{"election_id": eid, "member_id": oc.MembershipID})
	})
}

// ---------- results / ratify / annul / audit ----------

// Results — tally by position; winners fill position seats by count.
func (s *Service) Results(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) (map[string]any, error) {
	e, err := s.Get(ctx, oc, eid)
	if err != nil {
		return nil, err
	}
	if e.State != "concluded" && e.State != "ratified" {
		return nil, ErrBadState // no live tallies during voting
	}
	rows, err := s.q.TallyByPosition(ctx, toPg(eid))
	if err != nil {
		return nil, err
	}
	cands, err := s.q.ListCandidates(ctx, toPg(eid))
	if err != nil {
		return nil, err
	}
	names := map[uuid.UUID]string{}
	for _, c := range cands {
		names[fromPg(c.ID)] = c.PositionName + " — member " + pgTextStr(c.MemberNo)
	}
	byPos := map[string][]map[string]any{}
	for _, r := range rows {
		pid := fmt.Sprint(fromPg(r.PositionID))
		entry := map[string]any{"candidate_id": fromPg(r.CandidateID), "votes": r.Votes}
		if cid := fromPg(r.CandidateID); cid != uuid.Nil {
			entry["candidate"] = names[cid]
		} else {
			entry["candidate"] = "abstain"
		}
		byPos[pid] = append(byPos[pid], entry)
	}
	return map[string]any{"election_id": eid, "tally": byPos}, nil
}

func pgTextStr(t pgtype.Text) string {
	if t.Valid {
		return t.String
	}
	return ""
}

// Ratify — concluded → ratified: winners are seated for the elected term
// via the governance appointment seam (Rule 21 — grants activate when the
// term becomes current).
func (s *Service) Ratify(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) error {
	e, err := s.Get(ctx, oc, eid)
	if err != nil {
		return err
	}
	if e.State != "concluded" {
		return ErrBadState
	}
	if s.appointFn == nil {
		return fmt.Errorf("appoint seam not wired")
	}
	dates, err := s.q.TermDates(ctx, db.TermDatesParams{ID: e.TermID, OrganizationID: e.OrganizationID})
	if err != nil {
		return ErrNotFound
	}
	// winners: per position, candidates ranked by votes fill `seats`
	// seats; a tie AT the cutoff leaves that seat unfilled (audited).
	rows, err := s.q.TallyByPosition(ctx, e.ID)
	if err != nil {
		return err
	}
	type ranked struct {
		candidate pgtype.UUID
		votes     int64
		tie       bool
	}
	byPos := map[pgtype.UUID][]ranked{}
	for _, r := range rows {
		if !r.CandidateID.Valid {
			continue // abstain rows don't win seats
		}
		byPos[r.PositionID] = append(byPos[r.PositionID], ranked{candidate: r.CandidateID, votes: r.Votes})
	}
	positions, err := s.q.OrgPositions(ctx, e.OrganizationID)
	if err != nil {
		return err
	}
	seatsByPos := map[pgtype.UUID]int{}
	for _, p := range positions {
		seatsByPos[p.ID] = int(p.Seats)
	}
	winners := map[pgtype.UUID][]ranked{}
	for posID, list := range byPos {
		sort.Slice(list, func(i, j int) bool { return list[i].votes > list[j].votes })
		seats := seatsByPos[posID]
		if seats < 1 {
			seats = 1
		}
		var cut int64 = -1
		for i, c := range list {
			if i >= seats {
				break
			}
			if c.votes == 0 {
				break
			}
			// tie at the cutoff boundary → those seats stay empty
			if i > 0 && i == seats-1 && c.votes == list[i-1].votes {
				cut = c.votes
			}
		}
		for i, c := range list {
			if i >= seats || c.votes == 0 {
				break
			}
			if cut >= 0 && c.votes == cut {
				c.tie = true
			}
			winners[posID] = append(winners[posID], c)
		}
	}
	// candidate → membership + scope unit
	cands, err := s.q.ListCandidates(ctx, e.ID)
	if err != nil {
		return err
	}
	candMember := map[pgtype.UUID]pgtype.UUID{}
	candScope := map[pgtype.UUID]pgtype.UUID{}
	for _, c := range cands {
		candMember[c.ID] = c.MembershipID
		candScope[c.ID] = c.ScopeUnitID
	}
	// Appointments land first, on their own tx each (Rule 26 — governance
	// writes its own tables); the ratification marker then commits
	// atomically. If a seat fails, nothing is marked ratified.
	for posID, list := range winners {
		for _, w := range list {
			if w.tie {
				_ = s.withTx(ctx, func(q *db.Queries) error {
					return s.audit(ctx, q, oc, eid, "ratify_tie",
						map[string]any{"position_id": fromPg(posID), "votes": w.votes})
				})
				continue
			}
			memberID := candMember[w.candidate]
			if !memberID.Valid {
				continue
			}
			var branchID, committeeID uuid.UUID
			if scope := candScope[w.candidate]; scope.Valid {
				ut, err := s.q.OrgUnitType(ctx, db.OrgUnitTypeParams{
					ID: scope, OrganizationID: e.OrganizationID})
				if err != nil {
					return err
				}
				switch ut {
				case "branch":
					branchID = fromPg(scope)
				case "committee":
					committeeID = fromPg(scope)
				}
			}
			// idempotent re-ratify: an appointment already seated for this
			// term+position+member+scope means a prior run committed the seat
			// before a crash — skip rather than hit the seat-cap on retry.
			seated, err := s.q.AppointmentExists(ctx, db.AppointmentExistsParams{
				TermID: e.TermID, PositionID: posID, MemberID: memberID,
				BranchID: toPg(branchID), CommitteeID: toPg(committeeID)})
			if err != nil {
				return err
			}
			if seated {
				continue
			}
			if _, err := s.appointFn(ctx, oc, fromPg(e.TermID), fromPg(posID),
				fromPg(memberID), branchID, committeeID, dates.StartsAt.Time); err != nil {
				return err
			}
		}
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		m, err := q.SetElectionState(ctx, db.SetElectionStateParams{
			ID: e.ID, State: "ratified", State_2: "concluded"})
		if err != nil {
			return err
		}
		if m == 0 {
			return ErrBadState // concurrent ratify already landed
		}
		if err := s.audit(ctx, q, oc, eid, "ratified", nil); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ElectionRatified",
			"election", eid, oc.UserID, true,
			map[string]any{"election_id": eid, "term_id": fromPg(e.TermID)})
	})
}

// Annul — any live election can be voided with reason.
func (s *Service) Annul(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID, reason string) error {
	e, err := s.Get(ctx, oc, eid)
	if err != nil {
		return err
	}
	if e.State == "annulled" || e.State == "ratified" {
		return ErrBadState
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		m, err := q.SetElectionState(ctx, db.SetElectionStateParams{
			ID: e.ID, State: "annulled", State_2: e.State})
		if err != nil {
			return err
		}
		if m == 0 {
			return ErrBadState // concurrent transition already landed
		}
		if err := s.audit(ctx, q, oc, eid, "annulled", map[string]any{"reason": reason}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ElectionAnnulled",
			"election", eid, oc.UserID, true,
			map[string]any{"election_id": eid, "reason": reason})
	})
}

// AuditTrail — the election's append-only history.
func (s *Service) AuditTrail(ctx context.Context, oc *tenant.OrgCtx, eid uuid.UUID) ([]db.AuditEvents, error) {
	if _, err := s.Get(ctx, oc, eid); err != nil {
		return nil, err
	}
	return s.q.ElectionAuditTrail(ctx, db.ElectionAuditTrailParams{
		OrganizationID: toPg(oc.OrgID), TargetID: toPg(eid)})
}

// ---------- sweeps (elections:open_voting / close_voting / reminders) ----------

// OpenSweep — nominations/voting windows that arrived become live.
func (s *Service) OpenSweep(ctx context.Context) (int, error) {
	var n int
	noms, err := s.q.ElectionsToOpenNominations(ctx)
	if err != nil {
		return n, err
	}
	for _, e := range noms {
		err := s.withTx(ctx, func(q *db.Queries) error {
			m, err := q.SetElectionState(ctx, db.SetElectionStateParams{
				ID: e.ID, State: "nominations", State_2: "setup"})
			if err != nil || m == 0 {
				return err
			}
			tid, err := q.OrgTenantID(ctx, e.OrganizationID)
			if err != nil {
				return err
			}
			if err := s.auditRow(ctx, q, fromPg(tid), fromPg(e.OrganizationID), uuid.Nil,
				fromPg(e.ID), "nominations_opened", nil); err != nil {
				return err
			}
			return events.Emit(ctx, q, fromPg(tid), fromPg(e.OrganizationID),
				"NominationsOpened", "election", fromPg(e.ID), uuid.Nil, false,
				map[string]any{"election_id": fromPg(e.ID)})
		})
		if err == nil {
			n++
		}
	}
	votes, err := s.q.ElectionsToOpenVoting(ctx)
	if err != nil {
		return n, err
	}
	for _, e := range votes {
		err := s.withTx(ctx, func(q *db.Queries) error {
			m, err := q.SetElectionState(ctx, db.SetElectionStateParams{
				ID: e.ID, State: "voting", State_2: e.State})
			if err != nil || m == 0 {
				return err
			}
			tid, err := q.OrgTenantID(ctx, e.OrganizationID)
			if err != nil {
				return err
			}
			if err := s.auditRow(ctx, q, fromPg(tid), fromPg(e.OrganizationID), uuid.Nil,
				fromPg(e.ID), "voting_opened", nil); err != nil {
				return err
			}
			return events.Emit(ctx, q, fromPg(tid), fromPg(e.OrganizationID),
				"VotingOpened", "election", fromPg(e.ID), uuid.Nil, false,
				map[string]any{"election_id": fromPg(e.ID)})
		})
		if err == nil {
			n++
		}
	}
	return n, nil
}

// CloseSweep — voting windows past → conclude + emit VotingClosed /
// ElectionConcluded (idempotent via CAS).
func (s *Service) CloseSweep(ctx context.Context) (int, error) {
	rows, err := s.q.ElectionsToClose(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, e := range rows {
		err := s.withTx(ctx, func(q *db.Queries) error {
			m, err := q.SetElectionState(ctx, db.SetElectionStateParams{
				ID: e.ID, State: "concluded", State_2: "voting"})
			if err != nil || m == 0 {
				return err
			}
			tid, err := q.OrgTenantID(ctx, e.OrganizationID)
			if err != nil {
				return err
			}
			if err := s.auditRow(ctx, q, fromPg(tid), fromPg(e.OrganizationID), uuid.Nil,
				fromPg(e.ID), "voting_closed", nil); err != nil {
				return err
			}
			if err := events.Emit(ctx, q, fromPg(tid), fromPg(e.OrganizationID),
				"VotingClosed", "election", fromPg(e.ID), uuid.Nil, false,
				map[string]any{"election_id": fromPg(e.ID)}); err != nil {
				return err
			}
			return events.Emit(ctx, q, fromPg(tid), fromPg(e.OrganizationID),
				"ElectionConcluded", "election", fromPg(e.ID), uuid.Nil, true,
				map[string]any{"election_id": fromPg(e.ID)})
		})
		if err == nil {
			n++
		}
	}
	return n, nil
}

// RemindersSweep — elections:reminders: upcoming opens/closes emit a
// sourced ElectionReminder so members get the notice (catalogued).
func (s *Service) RemindersSweep(ctx context.Context) (int, error) {
	rows, err := s.q.ElectionsReminderWindow(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, e := range rows {
		sent, err := s.q.ReminderSentRecently(ctx, db.ReminderSentRecentlyParams{
			AggregateID: e.ID, Payload: []byte(e.State)})
		if err != nil || sent {
			continue // dedupe — one reminder per election+state per day
		}
		err = s.withTx(ctx, func(q *db.Queries) error {
			tid, err := q.OrgTenantID(ctx, e.OrganizationID)
			if err != nil {
				return err
			}
			return events.Emit(ctx, q, fromPg(tid), fromPg(e.OrganizationID),
				"ElectionReminder", "election", fromPg(e.ID), uuid.Nil, true,
				map[string]any{"election_id": fromPg(e.ID), "state": e.State,
					"voting_opens": e.VotingOpens.Time, "voting_closes": e.VotingCloses.Time})
		})
		if err == nil {
			n++
		}
	}
	return n, nil
}
