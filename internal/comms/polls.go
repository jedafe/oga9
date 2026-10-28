// Ref: oga9-engineering/sections/26-polls-surveys.md — polls are
// consultative (binding votes are §21 elections). Secrecy is structural:
// poll_ballots = turnout receipt, poll_votes = content; secret polls never
// stamp membership on votes. Anonymous surveys dedupe via the catalogued
// oga9:survey:claim marker — identity is never persisted on the row.
package comms

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrAlreadyVoted   = errors.New("comms: already_voted")
	ErrNotInAudience  = errors.New("comms: not_in_audience")
	ErrNotOpen        = errors.New("comms: not_open")
	ErrAlreadyRespond = errors.New("comms: already_responded")
)

// SetValkey wires the survey submit-claim marker store (catalogue:
// oga9:survey:claim:*) — nil in tests degrades to DB dedupe only.
func (s *Service) SetValkey(vk valkey.Client) { s.vk = vk }

// resolveAudience materializes an Audience spec into member ids — the
// eligibility snapshot stored on the poll/survey at open (§26).
func (s *Service) resolveAudience(ctx context.Context, orgID pgtype.UUID, aud Audience) ([]uuid.UUID, error) {
	units := make([]pgtype.UUID, len(aud.UnitIDs))
	for i, u := range aud.UnitIDs {
		units[i] = toPg(u)
	}
	rows, err := s.q.AudienceRecipients(ctx, db.AudienceRecipientsParams{
		OrganizationID: orgID, AllMembers: aud.All,
		Categories: aud.Categories, UnitIds: units, Positions: aud.Positions})
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, len(rows))
	for i, r := range rows {
		ids[i] = uuid.UUID(r.UserID.Bytes)
	}
	return ids, nil
}

// inAudience checks the open-time snapshot — resolved_members lands inside
// the row's audience blob; a member absent from it is not enfranchised.
func audienceMember(raw []byte, userID uuid.UUID) bool {
	var a struct {
		Resolved []uuid.UUID `json:"resolved_members"`
	}
	_ = json.Unmarshal(raw, &a)
	for _, id := range a.Resolved {
		if id == userID {
			return true
		}
	}
	return false
}

// notifyAudience fans an in_app notification to the resolved snapshot —
// gated on preferences, deduped on (user,kind,ref) like every fan-out.
func (s *Service) notifyAudience(ctx context.Context, q *db.Queries, e events.Event, kind, title, refType string, refID pgtype.UUID, memberUserIDs []uuid.UUID) error {
	for _, uid := range memberUserIDs {
		if err := s.notifyOnce(ctx, q, e, uid, kind, title, "", refType, refID); err != nil {
			return err
		}
	}
	return nil
}

// ---------- polls ----------

type PollIn struct {
	Question string    `json:"question"`
	Kind     string    `json:"kind"` // single|multi
	Options  []string  `json:"options"`
	Audience Audience  `json:"audience"`
	ClosesAt time.Time `json:"closes_at"`
}

func (s *Service) CreatePoll(ctx context.Context, oc *tenant.OrgCtx, in PollIn) (uuid.UUID, error) {
	if in.Question == "" || len(in.Options) < 2 {
		return uuid.Nil, ErrBadValue
	}
	if in.Kind != "single" && in.Kind != "multi" {
		return uuid.Nil, ErrBadValue
	}
	if !in.Audience.All && len(in.Audience.UnitIDs) == 0 &&
		len(in.Audience.Positions) == 0 && len(in.Audience.Categories) == 0 {
		in.Audience.All = true // empty spec = all members (announcement rule)
	}
	aj, _ := json.Marshal(in.Audience)
	var row db.Polls
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreatePoll(ctx, db.CreatePollParams{
			OrganizationID: toPg(oc.OrgID), CreatedBy: toPg(oc.MembershipID),
			Question: in.Question, Kind: in.Kind, Secret: false,
			Audience: aj, ClosesAt: tsPg(in.ClosesAt)})
		if err != nil {
			return err
		}
		for i, label := range in.Options {
			if label == "" {
				return ErrBadValue
			}
			if _, err := q.AddPollOption(ctx, db.AddPollOptionParams{
				PollID: row.ID, Label: label, Position: int32(i)}); err != nil {
				return err
			}
		}
		return nil
	})
	return fromPg(row.ID), err
}

func (s *Service) ListPolls(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.Polls, error) {
	return s.q.ListPolls(ctx, db.ListPollsParams{OrganizationID: toPg(oc.OrgID), Column2: state})
}

// GetPoll returns the poll + options + whether the caller already voted;
// tallies only appear once closed (results are a separate route).
func (s *Service) GetPoll(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) (*db.Polls, []db.PollOptions, bool, error) {
	p, err := s.q.GetPoll(ctx, db.GetPollParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, nil, false, ErrNotFound
	}
	opts, err := s.q.ListPollOptions(ctx, p.ID)
	if err != nil {
		return nil, nil, false, err
	}
	voted, err := s.q.HasBallot(ctx, db.HasBallotParams{PollID: p.ID, MembershipID: toPg(oc.MembershipID)})
	return &p, opts, voted, err
}

// OpenPoll — draft→open: secret resolves from the org rule (default false)
// and the audience snapshot is frozen into the row.
func (s *Service) OpenPoll(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		p, err := q.GetPoll(ctx, db.GetPollParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		var aud Audience
		_ = json.Unmarshal(p.Audience, &aud)
		secret := s.orgSettingBool(ctx, oc.OrgID, "org.comms.polls.secret_default", false)
		users, err := s.resolveAudience(ctx, p.OrganizationID, aud)
		if err != nil {
			return err
		}
		snap, _ := json.Marshal(map[string]any{"resolved_members": users})
		row, err := q.OpenPoll(ctx, db.OpenPollParams{
			ID: toPg(pid), OrganizationID: toPg(oc.OrgID),
			Secret: secret, ResolvedAudience: snap})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotOpen
		}
		if err != nil {
			return err
		}
		_ = row
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "PollOpened",
			"poll", pid, oc.UserID, false,
			map[string]any{"poll_id": pid, "org_id": oc.OrgID})
	})
}

// Vote casts a ballot + vote rows. Ballot-first ordering keeps a retried
// request honest: a replay hits the (poll_id,membership_id) unique and
// reports already_voted before any votes are written.
func (s *Service) Vote(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID, optionIDs []uuid.UUID) error {
	if len(optionIDs) == 0 {
		return ErrBadValue
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		p, err := q.GetPoll(ctx, db.GetPollParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		if p.State != "open" {
			return ErrNotOpen
		}
		if !audienceMember(p.Audience, oc.UserID) {
			return ErrNotInAudience
		}
		seen := map[uuid.UUID]bool{}
		var opts []pgtype.UUID
		for _, oid := range optionIDs {
			if seen[oid] {
				continue
			}
			seen[oid] = true
			ok, err := q.PollOptionBelongs(ctx, db.PollOptionBelongsParams{
				ID: toPg(oid), ID_2: p.ID, OrganizationID: toPg(oc.OrgID)})
			if err != nil || !ok {
				return ErrBadValue
			}
			opts = append(opts, toPg(oid))
		}
		if p.Kind == "single" && len(opts) > 1 {
			return ErrBadValue
		}
		b, err := q.CastBallot(ctx, db.CastBallotParams{PollID: p.ID, MembershipID: toPg(oc.MembershipID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAlreadyVoted
		}
		if err != nil {
			return err
		}
		// membership stamps only on non-secret polls — secrecy is structural.
		var member pgtype.UUID
		if !p.Secret {
			member = toPg(oc.MembershipID)
		}
		for _, oid := range opts {
			if err := q.AddPollVote(ctx, db.AddPollVoteParams{
				PollID: p.ID, OptionID: oid, MembershipID: member}); err != nil {
				return err
			}
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VoteCast",
			"poll", pid, oc.UserID, true,
			map[string]any{"poll_id": pid, "ballot_id": fromPg(b.ID)})
	})
}

// ClosePoll — manual close or the comms:close_due_polls sweep (shared
// CloseDuePolls path). Close emits PollClosed and notifies the snapshot.
func (s *Service) ClosePoll(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.ClosePoll(ctx, db.ClosePollParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotOpen
		}
		if err != nil {
			return err
		}
		return s.pollClosedTx(ctx, q, oc.TenantID, row)
	})
}

func (s *Service) pollClosedTx(ctx context.Context, q *db.Queries, tenantID uuid.UUID, p db.Polls) error {
	orgID := fromPg(p.OrganizationID)
	if tenantID == uuid.Nil {
		if t, err := q.GetOrgTenantID(ctx, p.OrganizationID); err == nil {
			tenantID = fromPg(t)
		}
	}
	return events.Emit(ctx, q, tenantID, orgID, "PollClosed",
		"poll", fromPg(p.ID), uuid.Nil, false,
		map[string]any{"poll_id": fromPg(p.ID)})
}

// CloseDuePolls — comms:close_due_polls sweep body.
func (s *Service) CloseDuePolls(ctx context.Context) (int, error) {
	due, err := s.q.DuePollsToClose(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, p := range due {
		err := s.withTx(ctx, func(q *db.Queries) error {
			row, err := q.ClosePoll(ctx, db.ClosePollParams{ID: p.ID, OrganizationID: p.OrganizationID})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil // already closed — idempotent
			}
			if err != nil {
				return err
			}
			return s.pollClosedTx(ctx, q, uuid.Nil, row)
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// PollResults — tallies reveal at close; open polls expose turnout only.
// Secret polls never return a voter list (there is none to return).
func (s *Service) PollResults(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) (map[string]any, error) {
	p, err := s.q.GetPoll(ctx, db.GetPollParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, ErrNotFound
	}
	turnout, err := s.q.PollTurnout(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"poll_id": pid, "state": p.State, "turnout": turnout}
	if p.State != "closed" {
		return out, nil // tallies reveal at close only
	}
	opts, _ := s.q.ListPollOptions(ctx, p.ID)
	tally, _ := s.q.PollTally(ctx, p.ID)
	per := map[string]int{}
	for _, t := range tally {
		per[uuid.UUID(t.OptionID.Bytes).String()] = int(t.Votes)
	}
	options := make([]map[string]any, len(opts))
	for i, o := range opts {
		options[i] = map[string]any{"option_id": uuid.UUID(o.ID.Bytes), "label": o.Label, "votes": per[uuid.UUID(o.ID.Bytes).String()]}
	}
	out["options"] = options
	if !p.Secret {
		voters, _ := s.q.PollVoters(ctx, p.ID)
		out["voters"] = voters
	}
	return out, nil
}

// ---------- surveys ----------

type SurveyField struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Type     string   `json:"type"` // text|number|bool|choice|multi
	Required bool     `json:"required"`
	Options  []string `json:"options"`
}

type SurveyIn struct {
	Title    string        `json:"title"`
	Intro    string        `json:"intro"`
	Schema   []SurveyField `json:"schema"`
	Audience Audience      `json:"audience"`
	ClosesAt time.Time     `json:"closes_at"`
}

func (s *Service) CreateSurvey(ctx context.Context, oc *tenant.OrgCtx, in SurveyIn) (uuid.UUID, error) {
	if in.Title == "" || len(in.Schema) == 0 {
		return uuid.Nil, ErrBadValue
	}
	if !in.Audience.All && len(in.Audience.UnitIDs) == 0 &&
		len(in.Audience.Positions) == 0 && len(in.Audience.Categories) == 0 {
		in.Audience.All = true
	}
	for _, f := range in.Schema {
		if f.Key == "" || f.Label == "" {
			return uuid.Nil, ErrBadValue
		}
	}
	sch, _ := json.Marshal(in.Schema)
	aj, _ := json.Marshal(in.Audience)
	row, err := s.q.CreateSurvey(ctx, db.CreateSurveyParams{
		OrganizationID: toPg(oc.OrgID), CreatedBy: toPg(oc.MembershipID),
		Title: in.Title, Intro: pgText(in.Intro), Schema: sch,
		Anonymous: false, Audience: aj, ClosesAt: tsPg(in.ClosesAt)})
	return fromPg(row.ID), err
}

func (s *Service) ListSurveys(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.Surveys, error) {
	return s.q.ListSurveys(ctx, db.ListSurveysParams{OrganizationID: toPg(oc.OrgID), Column2: state})
}

func (s *Service) GetSurvey(ctx context.Context, oc *tenant.OrgCtx, sid uuid.UUID) (db.Surveys, bool, error) {
	sv, err := s.q.GetSurvey(ctx, db.GetSurveyParams{ID: toPg(sid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return db.Surveys{}, false, ErrNotFound
	}
	_, err = s.q.MySurveyResponse(ctx, db.MySurveyResponseParams{
		SurveyID: sv.ID, MembershipID: toPg(oc.MembershipID)})
	return sv, err == nil, nil
}

func (s *Service) OpenSurvey(ctx context.Context, oc *tenant.OrgCtx, sid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		sv, err := q.GetSurvey(ctx, db.GetSurveyParams{ID: toPg(sid), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		var aud Audience
		_ = json.Unmarshal(sv.Audience, &aud)
		anon := s.orgSettingBool(ctx, oc.OrgID, "org.surveys.anonymous_default", false)
		users, err := s.resolveAudience(ctx, sv.OrganizationID, aud)
		if err != nil {
			return err
		}
		snap, _ := json.Marshal(map[string]any{"resolved_members": users})
		row, err := q.OpenSurvey(ctx, db.OpenSurveyParams{
			ID: toPg(sid), OrganizationID: toPg(oc.OrgID),
			Anonymous: anon, ResolvedAudience: snap})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotOpen
		}
		if err != nil {
			return err
		}
		_ = row
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "SurveyOpened",
			"survey", sid, oc.UserID, false,
			map[string]any{"survey_id": sid, "org_id": oc.OrgID})
	})
}

// Respond — schema validation (required keys, choice membership) then the
// insert. Anonymous surveys: membership stays NULL and dedupe is the
// oga9:survey:claim marker (hash checked at write, identity not stored).
func (s *Service) Respond(ctx context.Context, oc *tenant.OrgCtx, sid uuid.UUID, answers map[string]any) error {
	sv, err := s.q.GetSurvey(ctx, db.GetSurveyParams{ID: toPg(sid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if sv.State != "open" {
		return ErrNotOpen
	}
	if !audienceMember(sv.Audience, oc.UserID) {
		return ErrNotInAudience
	}
	var schema []SurveyField
	_ = json.Unmarshal(sv.Schema, &schema)
	for _, f := range schema {
		v, ok := answers[f.Key]
		if !ok || v == nil {
			if f.Required {
				return ErrBadValue
			}
			continue
		}
		if f.Type == "choice" && len(f.Options) > 0 {
			vs, _ := v.(string)
			found := false
			for _, o := range f.Options {
				if o == vs {
					found = true
				}
			}
			if !found {
				return ErrBadValue
			}
		}
	}
	if sv.Anonymous && s.vk != nil {
		h := sha256.Sum256([]byte(oc.MembershipID.String()))
		claim := "oga9:survey:claim:" + sid.String() + ":" + hex.EncodeToString(h[:])
		ttl := 7 * 24 * time.Hour
		if sv.ClosesAt.Valid {
			if d := time.Until(sv.ClosesAt.Time); d > ttl {
				ttl = d
				if ttl > 90*24*time.Hour {
					ttl = 90 * 24 * time.Hour
				}
			}
		}
		ok, err := s.vk.Do(ctx, s.vk.B().Set().Key(claim).Value("1").Nx().Ex(ttl).Build()).AsBool()
		if err == nil && !ok {
			return ErrAlreadyRespond
		}
	}
	var member pgtype.UUID
	if !sv.Anonymous {
		member = toPg(oc.MembershipID)
	}
	aj, _ := json.Marshal(answers)
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.SubmitSurveyResponse(ctx, db.SubmitSurveyResponseParams{
			SurveyID: sv.ID, MembershipID: member, Answers: aj})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAlreadyRespond
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "SurveyResponseSubmitted",
			"survey", sid, oc.UserID, false,
			map[string]any{"survey_id": sid, "response_id": fromPg(row.ID)})
	})
}

func (s *Service) CloseSurvey(ctx context.Context, oc *tenant.OrgCtx, sid uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.CloseSurvey(ctx, db.CloseSurveyParams{ID: toPg(sid), OrganizationID: toPg(oc.OrgID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotOpen
		}
		if err != nil {
			return err
		}
		return s.surveyClosedTx(ctx, q, oc.TenantID, row)
	})
}

func (s *Service) surveyClosedTx(ctx context.Context, q *db.Queries, tenantID uuid.UUID, sv db.Surveys) error {
	orgID := fromPg(sv.OrganizationID)
	if tenantID == uuid.Nil {
		if t, err := q.GetOrgTenantID(ctx, sv.OrganizationID); err == nil {
			tenantID = fromPg(t)
		}
	}
	return events.Emit(ctx, q, tenantID, orgID, "SurveyClosed",
		"survey", fromPg(sv.ID), uuid.Nil, false,
		map[string]any{"survey_id": fromPg(sv.ID)})
}

// CloseDueSurveys — comms:close_due_surveys sweep body.
func (s *Service) CloseDueSurveys(ctx context.Context) (int, error) {
	due, err := s.q.DueSurveysToClose(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, sv := range due {
		err := s.withTx(ctx, func(q *db.Queries) error {
			row, err := q.CloseSurvey(ctx, db.CloseSurveyParams{ID: sv.ID, OrganizationID: sv.OrganizationID})
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			return s.surveyClosedTx(ctx, q, uuid.Nil, row)
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// SurveyResults aggregates answers per schema field — choice/multi get
// option counts; everything else counts submissions. Anonymous rows carry
// no membership, so nothing linkable is ever returned.
func (s *Service) SurveyResults(ctx context.Context, oc *tenant.OrgCtx, sid uuid.UUID) (map[string]any, error) {
	sv, err := s.q.GetSurvey(ctx, db.GetSurveyParams{ID: toPg(sid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, ErrNotFound
	}
	rows, err := s.q.ListSurveyResponses(ctx, sv.ID)
	if err != nil {
		return nil, err
	}
	var schema []SurveyField
	_ = json.Unmarshal(sv.Schema, &schema)
	fields := make([]map[string]any, len(schema))
	for i, f := range schema {
		counts := map[string]int{}
		answered := 0
		for _, r := range rows {
			var ans map[string]any
			_ = json.Unmarshal(r.Answers, &ans)
			v, ok := ans[f.Key]
			if !ok {
				continue
			}
			answered++
			switch vv := v.(type) {
			case string:
				counts[vv]++
			case []any:
				for _, x := range vv {
					if s2, ok := x.(string); ok {
						counts[s2]++
					}
				}
			}
		}
		fields[i] = map[string]any{"key": f.Key, "label": f.Label,
			"type": f.Type, "answered": answered, "counts": counts}
	}
	return map[string]any{"survey_id": sid, "state": sv.State,
		"responses": len(rows), "fields": fields}, nil
}

// PollSurveyNotifyConsumer — dispatcher consumer for PollOpened/PollClosed/
// SurveyOpened/SurveyClosed. Reads the open-time audience snapshot and fans
// notifications out here (Rule 8: never in the request tx). notifyOnce
// dedupes on (user,kind,ref) → at-least-once safe.
func (s *Service) PollSurveyNotifyConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	var title, kind string
	var refID pgtype.UUID
	var aud []byte
	switch e.Type {
	case "PollOpened", "PollClosed":
		p, err := q.GetPoll(ctx, db.GetPollParams{ID: toPg(e.AggregateID), OrganizationID: toPg(e.OrganizationID)})
		if err != nil {
			return nil
		}
		kind, refID, aud = "poll", p.ID, p.Audience
		if e.Type == "PollOpened" {
			title = "New poll: " + p.Question
		} else {
			title = "Poll closed: " + p.Question
		}
	case "SurveyOpened", "SurveyClosed":
		sv, err := q.GetSurvey(ctx, db.GetSurveyParams{ID: toPg(e.AggregateID), OrganizationID: toPg(e.OrganizationID)})
		if err != nil {
			return nil
		}
		kind, refID, aud = "survey", sv.ID, sv.Audience
		if e.Type == "SurveyOpened" {
			title = "New survey: " + sv.Title
		} else {
			title = "Survey closed: " + sv.Title
		}
	default:
		return nil
	}
	var snap struct {
		Resolved []uuid.UUID `json:"resolved_members"`
	}
	_ = json.Unmarshal(aud, &snap)
	return s.notifyAudience(ctx, q, e, kind, title, kind, refID, snap.Resolved)
}

func tsPg(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
