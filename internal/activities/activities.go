// Ref: oga9-engineering/sections/13-events-meetings.md — events & meetings.
// Meetings are governance, events are social (Rule 22); notice/quorum/fine
// rules resolve through org_settings (Rule 23); every state change emits
// its domain event in the same tx (Rule 12).
package activities

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/rules"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound          = errors.New("activities: not_found")
	ErrInvalidTransition = errors.New("activities: invalid_state_transition")
	ErrEventFull         = errors.New("activities: event_full")
	ErrBadValue          = errors.New("activities: invalid_value")
	ErrBadKind           = errors.New("activities: invalid_kind")
	ErrAlreadyRegistered = errors.New("activities: already_registered")
	ErrGuestLimit        = errors.New("activities: guest_limit")
)

type Service struct {
	ru   *rules.Service // ADR-016 rules engine — org_settings resolve through it (Rule 23)
	pool *pgxpool.Pool
	q    *db.Queries
	// fineHook posts a penalty obligation in finance (Rule 26: activities
	// never writes finance tables — SetFineHook wires finance.AssessFine).
	fineFn func(ctx context.Context, orgID, memberID uuid.UUID, amount int64, currency, period string)
	// fineRevokeFn waives a previously assessed fine when a mark is
	// corrected away from late/absent — wired to finance.RevokeFine.
	fineRevokeFn func(ctx context.Context, orgID, memberID uuid.UUID, period string)
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{ru: rules.New(pool), pool: pool, q: db.New(pool)}
}

// SetFineHook wires the finance fine-assessment seam.
func (s *Service) SetFineHook(fn func(ctx context.Context, orgID, memberID uuid.UUID, amount int64, currency, period string)) {
	s.fineFn = fn
}

// SetFineRevokeHook wires the finance fine-revocation seam for corrected
// attendance marks.
func (s *Service) SetFineRevokeHook(fn func(ctx context.Context, orgID, memberID uuid.UUID, period string)) {
	s.fineRevokeFn = fn
}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }
func pgTs(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: !t.IsZero()}
}
func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: !t.IsZero()} }
func int4ptr(p *int) pgtype.Int4 {
	if p == nil {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(*p), Valid: true}
}

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

func (s *Service) orgSettingBool(ctx context.Context, orgID uuid.UUID, key string, fallback bool) bool {
	raw, err := s.ru.Value(ctx, orgID, key)
	if err != nil {
		return fallback
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return fallback
	}
	return b
}

func (s *Service) orgSettingInt(ctx context.Context, orgID uuid.UUID, key string, fallback int64) int64 {
	raw, err := s.ru.Value(ctx, orgID, key)
	if err != nil {
		return fallback
	}
	var n json.Number
	if json.Unmarshal(raw, &n) != nil {
		return fallback
	}
	v, err := n.Int64()
	if err != nil {
		return fallback
	}
	return v
}

// ---------- activities spine ----------

type ActivityIn struct {
	Kind        string          `json:"kind"` // meeting|event
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Venue       string          `json:"venue"`
	StartsAt    time.Time       `json:"starts_at"`
	EndsAt      time.Time       `json:"ends_at"`
	UnitID      uuid.UUID       `json:"unit_id"`
	QuorumPct   *int            `json:"quorum_percent"`
	Capacity    *int            `json:"capacity"`
	Agenda      json.RawMessage `json:"agenda"` // meetings: [{order,title,mover,duration}]
}

// CreateActivity validates kind + scope, resolves quorum/notice rules from
// org_settings, and emits MeetingScheduled|EventPublished. Returns
// warn=true when the start lands inside the notice window (§13: warn, not
// block — the caller surfaces an override flag to the client).
func (s *Service) CreateActivity(ctx context.Context, oc *tenant.OrgCtx, in ActivityIn) (uuid.UUID, bool, error) {
	if in.Kind != "meeting" && in.Kind != "event" {
		return uuid.Nil, false, ErrBadKind
	}
	if in.Title == "" || in.StartsAt.IsZero() {
		return uuid.Nil, false, ErrBadValue
	}
	if in.UnitID != uuid.Nil {
		if u, err := s.q.GetOrgUnit(ctx, db.GetOrgUnitParams{ID: toPg(in.UnitID), OrganizationID: toPg(oc.OrgID)}); err != nil {
			return uuid.Nil, false, ErrNotFound
		} else {
			_ = u
		}
	}
	var quorum pgtype.Int4
	if in.Kind == "meeting" {
		qp := in.QuorumPct
		if qp == nil {
			d := int(s.orgSettingInt(ctx, oc.OrgID, "org.meeting.quorum_percent", 0))
			if d > 0 {
				qp = &d
			}
		}
		if qp != nil {
			quorum = pgtype.Int4{Int32: int32(*qp), Valid: true}
		}
	}
	var capacity pgtype.Int4
	if in.Kind == "event" && in.Capacity != nil {
		capacity = pgtype.Int4{Int32: int32(*in.Capacity), Valid: true}
	}
	warn := false
	if in.Kind == "meeting" {
		notice := s.orgSettingInt(ctx, oc.OrgID, "org.meeting.notice_days", 0)
		if notice > 0 && in.StartsAt.Before(time.Now().Add(time.Duration(notice)*24*time.Hour)) {
			warn = true // inside notice window — flagged, not blocked
		}
	}
	var row db.Activities
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateActivity(ctx, db.CreateActivityParams{
			OrganizationID: toPg(oc.OrgID), Kind: in.Kind, Title: in.Title,
			Description: pgText(in.Description), Venue: pgText(in.Venue),
			StartsAt: pgTs(in.StartsAt), EndsAt: pgTs(in.EndsAt),
			UnitID: toPg(in.UnitID), QuorumPercent: quorum, Capacity: capacity,
			CreatedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		if in.Kind == "meeting" && len(in.Agenda) > 0 {
			if _, err := q.UpsertAgenda(ctx, db.UpsertAgendaParams{ActivityID: row.ID, Items: in.Agenda}); err != nil {
				return err
			}
			// an agenda supplied at create publishes like a later upsert.
			if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AgendaPublished",
				"activity", fromPg(row.ID), oc.UserID, false,
				map[string]any{"activity_id": fromPg(row.ID)}); err != nil {
				return err
			}
		}
		ev := "EventPublished"
		if in.Kind == "meeting" {
			ev = "MeetingScheduled"
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev, "activity", fromPg(row.ID), oc.UserID, false,
			map[string]any{"activity_id": fromPg(row.ID), "org_id": oc.OrgID,
				"kind": in.Kind, "starts_at": in.StartsAt, "notice_warning": warn})
	})
	if err != nil {
		return uuid.Nil, false, err
	}
	return fromPg(row.ID), warn, nil
}

func (s *Service) List(ctx context.Context, oc *tenant.OrgCtx, kind, status string, unitID uuid.UUID) ([]db.Activities, error) {
	return s.q.ListActivities(ctx, db.ListActivitiesParams{
		OrganizationID: toPg(oc.OrgID), Column2: kind, Column3: status, Column4: toPg(unitID)})
}

func (s *Service) Get(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) (*db.Activities, error) {
	a, err := s.q.GetActivity(ctx, db.GetActivityParams{ID: toPg(aid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, ErrNotFound
	}
	return &a, nil
}

// PatchActivity edits a live activity; status transitions go through it too
// (completed/cancelled emit their events; cancelled is terminal). An
// agenda payload on a meeting upserts meeting_agendas + AgendaPublished.
// clearUnit strips the unit scope (PATCH {unit_id: null} can't express
// itself through a COALESCE update). Returns the re-evaluated notice
// warning when starts_at moved.
func (s *Service) PatchActivity(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID, in ActivityIn, status *string, clearUnit bool) (bool, error) {
	var st pgtype.Text
	if status != nil {
		switch *status {
		case "scheduled", "completed", "cancelled":
		default:
			return false, ErrBadValue
		}
		st = pgText(*status)
	}
	cur, err := s.Get(ctx, oc, aid)
	if err != nil {
		return false, err
	}
	if cur.Status == "cancelled" {
		// cancelled is terminal — field edits and status moves both refuse
		// (the UPDATE's status<>'cancelled' filter would otherwise 404).
		return false, ErrInvalidTransition
	}
	if status != nil && cur.Status != "scheduled" {
		// completed/cancelled are terminal — no status moves off them.
		return false, ErrInvalidTransition
	}
	warn := false
	if cur.Kind == "meeting" && !in.StartsAt.IsZero() {
		// reschedule re-evaluates the notice window (§13 warn-not-block).
		notice := s.orgSettingInt(ctx, oc.OrgID, "org.meeting.notice_days", 0)
		if notice > 0 && in.StartsAt.Before(time.Now().Add(time.Duration(notice)*24*time.Hour)) {
			warn = true
		}
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.UpdateActivity(ctx, db.UpdateActivityParams{
			ID: toPg(aid), OrganizationID: toPg(oc.OrgID),
			Title: pgText(in.Title), Description: pgText(in.Description), Venue: pgText(in.Venue),
			StartsAt: pgTs(in.StartsAt), EndsAt: pgTs(in.EndsAt), UnitID: toPg(in.UnitID),
			QuorumPercent: int4ptr(in.QuorumPct), Capacity: int4ptr(in.Capacity),
			Status: st, ClearUnit: clearUnit})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if len(in.Agenda) > 0 {
			if cur.Kind != "meeting" {
				return ErrBadKind // agendas only exist on meetings (Rule 22)
			}
			if _, err := q.UpsertAgenda(ctx, db.UpsertAgendaParams{ActivityID: cur.ID, Items: in.Agenda}); err != nil {
				return err
			}
			if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AgendaPublished",
				"activity", aid, oc.UserID, false, map[string]any{"activity_id": aid}); err != nil {
				return err
			}
		}
		if status != nil && *status == "cancelled" {
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ActivityCancelled",
				"activity", aid, oc.UserID, false, map[string]any{"activity_id": aid})
		}
		if status != nil && *status == "completed" && cur.Kind == "event" {
			// EventCompleted is event-domain; meetings closing emit nothing.
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "EventCompleted",
				"activity", aid, oc.UserID, false, map[string]any{"event_id": aid, "org_id": oc.OrgID})
		}
		return nil
	})
	return warn, err
}

func (s *Service) OrgCalendar(ctx context.Context, oc *tenant.OrgCtx, unitID uuid.UUID) ([]db.Activities, error) {
	return s.q.OrgCalendar(ctx, db.OrgCalendarParams{OrganizationID: toPg(oc.OrgID), Column2: toPg(unitID)})
}

func (s *Service) MyCalendar(ctx context.Context, userID uuid.UUID) ([]db.Activities, error) {
	return s.q.MyCalendar(ctx, toPg(userID))
}

// ---------- agendas / minutes ----------

func (s *Service) SetAgenda(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID, items json.RawMessage) error {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return err
	}
	if a.Kind != "meeting" {
		return ErrBadKind
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.UpsertAgenda(ctx, db.UpsertAgendaParams{ActivityID: a.ID, Items: items}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AgendaPublished",
			"activity", aid, oc.UserID, false, map[string]any{"activity_id": aid})
	})
}

func (s *Service) GetAgenda(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) (json.RawMessage, error) {
	if _, err := s.Get(ctx, oc, aid); err != nil {
		return nil, err
	}
	g, err := s.q.GetAgenda(ctx, toPg(aid))
	if err != nil {
		return json.RawMessage("[]"), nil
	}
	return g.Items, nil
}

// DraftMinutes appends a new version; approved minutes stay immutable.
func (s *Service) DraftMinutes(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID, body string) (int32, error) {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return 0, err
	}
	if a.Kind != "meeting" || body == "" {
		return 0, ErrBadValue
	}
	var row db.Minutes
	err = s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateMinutes(ctx, db.CreateMinutesParams{ActivityID: a.ID, Body: body})
		if err == pgx.ErrNoRows {
			row, err = q.CreateMinutesFirst(ctx, db.CreateMinutesFirstParams{ActivityID: a.ID, Body: body})
		}
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrInvalidTransition // concurrent draft — UNIQUE(activity_id,version)
			}
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MinutesDrafted",
			"minutes", fromPg(row.ID), oc.UserID, false,
			map[string]any{"minutes_id": fromPg(row.ID), "activity_id": aid, "version": row.Version})
	})
	return row.Version, err
}

// ApproveMinutes locks a draft version and archives any older approved one —
// entity_links edges + documents archive are downstream consumers (g11).
func (s *Service) ApproveMinutes(ctx context.Context, oc *tenant.OrgCtx, aid, minutesID uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		m, err := q.ApproveMinutes(ctx, db.ApproveMinutesParams{ID: toPg(minutesID), ApprovedBy: toPg(oc.MembershipID), OrganizationID: toPg(oc.OrgID)})
		if err == pgx.ErrNoRows {
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		if fromPg(m.ActivityID) != aid {
			// the URL's meeting must own the minutes row — otherwise the
			// emitted MinutesApproved would record the wrong meeting.
			return ErrNotFound
		}
		if _, err := q.ArchiveMinutes(ctx, db.ArchiveMinutesParams{ActivityID: m.ActivityID, ID: m.ID}); err != nil {
			return err
		}
		// The payload carries everything downstream consumers need —
		// comms archives to documents straight from the event without
		// reading this module's tables (Rule 26).
		a, err := q.GetActivity(ctx, db.GetActivityParams{ID: m.ActivityID, OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return err
		}
		pl := map[string]any{
			"minutes_id": minutesID, "meeting_id": aid, "body": m.Body,
			"meeting_title": a.Title, "starts_at": a.StartsAt.Time,
			"approved_by": oc.MembershipID}
		if a.UnitID.Valid {
			pl["unit_id"] = fromPg(a.UnitID)
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MinutesApproved",
			"minutes", minutesID, oc.UserID, true, pl)
	})
}

func (s *Service) Minutes(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) ([]db.Minutes, error) {
	return s.q.ListMinutes(ctx, db.ListMinutesParams{ActivityID: toPg(aid), OrganizationID: toPg(oc.OrgID)})
}

// ---------- action items ----------

func (s *Service) CreateActionItem(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID, assignee uuid.UUID, title string, due time.Time) (uuid.UUID, error) {
	if _, err := s.Get(ctx, oc, aid); err != nil {
		return uuid.Nil, err
	}
	// assignee must be a member of this org
	if org, err := s.q.GetMembershipOrg(ctx, toPg(assignee)); err != nil || fromPg(org) != oc.OrgID {
		return uuid.Nil, ErrNotFound
	}
	var row db.ActionItems
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		row, err = q.CreateActionItem(ctx, db.CreateActionItemParams{
			ActivityID: toPg(aid), AssigneeID: toPg(assignee), Title: title, DueDate: pgDate(due)})
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ActionItemAssigned",
			"action_item", fromPg(row.ID), oc.UserID, false,
			map[string]any{"item_id": fromPg(row.ID), "assignee_id": assignee})
	})
	return fromPg(row.ID), err
}

// ActionItems — assignee=Nil lists the whole sheet (manage holders);
// a concrete id scopes to that assignee (§13 view_own for non-managers).
func (s *Service) ActionItems(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID, state string, assignee uuid.UUID) ([]db.ActionItems, error) {
	return s.q.ListActionItems(ctx, db.ListActionItemsParams{
		ActivityID: toPg(aid), Column2: state, OrganizationID: toPg(oc.OrgID), Column4: toPg(assignee)})
}

func (s *Service) MyActionItems(ctx context.Context, userID uuid.UUID) ([]db.ActionItems, error) {
	return s.q.MyActionItems(ctx, toPg(userID))
}

// CompleteActionItem — self path scopes to the caller's membership row.
// Emits ActionItemCompleted (§13 catalog) in the same tx.
func (s *Service) CompleteActionItem(ctx context.Context, oc *tenant.OrgCtx, itemID uuid.UUID, self bool) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		var n int64
		var err error
		if self {
			n, err = q.CompleteActionItem(ctx, db.CompleteActionItemParams{ID: toPg(itemID), AssigneeID: toPg(oc.MembershipID), OrganizationID: toPg(oc.OrgID)})
		} else {
			n, err = q.CompleteActionItemManage(ctx, db.CompleteActionItemManageParams{ID: toPg(itemID), OrganizationID: toPg(oc.OrgID)})
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrInvalidTransition
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ActionItemCompleted",
			"action_item", itemID, oc.UserID, false, map[string]any{"item_id": itemID})
	})
}

func (s *Service) CancelActionItem(ctx context.Context, oc *tenant.OrgCtx, itemID uuid.UUID) error {
	n, err := s.q.CancelActionItem(ctx, db.CancelActionItemParams{ID: toPg(itemID), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalidTransition
	}
	return nil
}

func (s *Service) PatchActionItem(ctx context.Context, oc *tenant.OrgCtx, itemID uuid.UUID, title string, assignee uuid.UUID, due time.Time) error {
	if assignee != uuid.Nil {
		if org, err := s.q.GetMembershipOrg(ctx, toPg(assignee)); err != nil || fromPg(org) != oc.OrgID {
			return ErrNotFound
		}
	}
	n, err := s.q.PatchActionItem(ctx, db.PatchActionItemParams{
		ID: toPg(itemID), Title: pgText(title), AssigneeID: toPg(assignee), DueDate: pgDate(due),
		OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrInvalidTransition
	}
	return nil
}

// ---------- attendance (bulk sheet → fines via rules engine) ----------

type MarkIn struct {
	MembershipID uuid.UUID `json:"membership_id"`
	Status       string    `json:"status"` // present|absent|excused|late
	Note         string    `json:"note"`
}

// MarkAttendance bulk-marks the sheet; each mark emits AttendanceRecorded
// and late/absent rows feed per-org fines through the finance seam
// (org.attendance.late_fine_minor / absent_fine_minor, Rule 23).
func (s *Service) MarkAttendance(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID, marks []MarkIn) error {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return err
	}
	if a.Kind != "meeting" {
		return ErrBadKind
	}
	if a.Status == "cancelled" {
		return ErrInvalidTransition
	}
	now := time.Now()
	lateFine := s.orgSettingInt(ctx, oc.OrgID, "org.attendance.late_fine_minor", 0)
	absentFine := s.orgSettingInt(ctx, oc.OrgID, "org.attendance.absent_fine_minor", 0)
	err = s.withTx(ctx, func(q *db.Queries) error {
		for _, m := range marks {
			switch m.Status {
			case "present", "absent", "excused", "late":
			default:
				return ErrBadValue
			}
			if org, err := q.GetMembershipOrg(ctx, toPg(m.MembershipID)); err != nil || fromPg(org) != oc.OrgID {
				return ErrNotFound // attendance only for this org's members
			}
			if err := q.MarkAttendance(ctx, db.MarkAttendanceParams{
				ActivityID: a.ID, MembershipID: toPg(m.MembershipID), Status: m.Status,
				CheckedAt: pgTs(now), Note: pgText(m.Note)}); err != nil {
				return err
			}
			if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AttendanceRecorded",
				"activity", aid, oc.UserID, false,
				map[string]any{"meeting_id": aid, "member_id": m.MembershipID, "status": m.Status}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	// fines post-commit: a fine write must not roll back the sheet.
	s.fineAssess(oc.OrgID, marks, aid, lateFine, absentFine)
	return nil
}

func (s *Service) fineAssess(orgID uuid.UUID, marks []MarkIn, aid uuid.UUID, lateFine, absentFine int64) {
	if s.fineFn == nil || (lateFine <= 0 && absentFine <= 0) {
		return
	}
	cur := "NGN"
	if raw, err := s.ru.Value(context.Background(), orgID, "org.dues.currency"); err == nil {
		var c string
		if json.Unmarshal(raw, &c) == nil && c != "" {
			cur = c
		}
	}
	period := "attendance:" + aid.String() // full id — an 8-char prefix collides at scale
	for _, m := range marks {
		switch m.Status {
		case "late":
			if lateFine > 0 {
				s.fineFn(context.Background(), orgID, m.MembershipID, lateFine, cur, period)
			}
		case "absent":
			if absentFine > 0 {
				s.fineFn(context.Background(), orgID, m.MembershipID, absentFine, cur, period)
			}
		default:
			// present|excused re-mark: a previously assessed fine for this
			// meeting is revoked — the correction must clear the penalty.
			if s.fineRevokeFn != nil {
				s.fineRevokeFn(context.Background(), orgID, m.MembershipID, period)
			}
		}
	}
}

func (s *Service) AttendanceSheet(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) ([]db.Attendances, error) {
	if _, err := s.Get(ctx, oc, aid); err != nil {
		return nil, err
	}
	return s.q.ListAttendance(ctx, toPg(aid))
}

// Quorum — present+late / eligible members vs quorum_percent (Rule 23).
func (s *Service) EvaluateQuorum(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) (bool, int, error) {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return false, 0, err
	}
	if a.Kind != "meeting" {
		return false, 0, ErrBadKind
	}
	present, err := s.q.AttendanceCount(ctx, a.ID)
	if err != nil {
		return false, 0, err
	}
	// Denominator: unit roster when the meeting is unit-scoped, else the
	// org's active+life_member count (quorum_percent means "% of whom").
	var eligible int64
	if a.UnitID.Valid {
		eligible, err = s.q.CountUnitMembers(ctx, db.CountUnitMembersParams{
			OrganizationID: a.OrganizationID, Column2: a.UnitID})
	} else {
		eligible, err = s.q.CountOrgMembers(ctx, toPg(oc.OrgID))
	}
	if err != nil || eligible == 0 {
		return false, 0, err
	}
	pct := int(present) * 100 / int(eligible)
	met := !a.QuorumPercent.Valid || pct >= int(a.QuorumPercent.Int32)
	err = s.withTx(ctx, func(q *db.Queries) error {
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "QuorumEvaluated",
			"activity", aid, oc.UserID, true,
			map[string]any{"activity_id": aid, "met": met, "percent": pct})
	})
	if err != nil {
		return false, 0, err // a swallowed emit would drop the sourced event
	}
	return met, pct, nil
}

// ---------- events: registration + check-in ----------

// RegisterEvent — member path (membership_id set) or guest (name+contact).
// Capacity counts live registrations; full → event_full (§13 scenario 5).
func (s *Service) RegisterEvent(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID, memberID uuid.UUID, guestName, guestContact string) (uuid.UUID, error) {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return uuid.Nil, err
	}
	if a.Kind != "event" {
		return uuid.Nil, ErrBadKind
	}
	if memberID == uuid.Nil && guestName == "" {
		return uuid.Nil, ErrBadValue // anonymous guests aren't registrations
	}
	var row db.EventRegistrations
	err = s.withTx(ctx, func(q *db.Queries) error {
		locked, err := q.GetActivityForUpdate(ctx, a.ID) // serializes registration
		if err != nil {
			return err
		}
		if locked.Status != "scheduled" {
			return ErrInvalidTransition
		}
		if memberID == uuid.Nil {
			// Guest registration — capped per member so one member can't
			// stuff the roster (org.events.guest_cap; 0 = unlimited).
			if cap := s.orgSettingInt(ctx, oc.OrgID, "org.events.guest_cap", 0); cap > 0 {
				n, err := q.GuestCountByMember(ctx, db.GuestCountByMemberParams{
					ActivityID: a.ID, RegisteredBy: toPg(oc.MembershipID)})
				if err != nil {
					return err
				}
				if n >= cap {
					return ErrGuestLimit
				}
			}
		}
		if locked.Capacity.Valid {
			n, err := q.EventHeadcount(ctx, locked.ID)
			if err != nil {
				return err
			}
			if int(n) >= int(locked.Capacity.Int32) {
				return ErrEventFull
			}
		}
		row, err = q.RegisterEvent(ctx, db.RegisterEventParams{
			ActivityID: a.ID, MembershipID: toPg(memberID),
			GuestName: pgText(guestName), GuestContact: pgText(guestContact),
			RegisteredBy: toPg(oc.MembershipID)})
		if err == pgx.ErrNoRows {
			return ErrAlreadyRegistered // member re-register dedupe
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "EventRegistrationConfirmed",
			"event_registration", fromPg(row.ID), oc.UserID, false,
			map[string]any{"registration_id": fromPg(row.ID), "event_id": aid})
	})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(row.ID), nil
}

// CheckIn — members check in by membership_id; guests (no membership)
// check in by their registration_id.
func (s *Service) CheckIn(ctx context.Context, oc *tenant.OrgCtx, aid, memberID, registrationID uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		var r db.EventRegistrations
		var err error
		if registrationID != uuid.Nil {
			r, err = q.CheckInByID(ctx, db.CheckInByIDParams{ID: toPg(registrationID), ActivityID: toPg(aid), OrganizationID: toPg(oc.OrgID)})
		} else {
			r, err = q.CheckIn(ctx, db.CheckInParams{ActivityID: toPg(aid), MembershipID: toPg(memberID), OrganizationID: toPg(oc.OrgID)})
		}
		if err == pgx.ErrNoRows {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CheckedIn",
			"event_registration", fromPg(r.ID), oc.UserID, false,
			map[string]any{"registration_id": fromPg(r.ID), "member_id": memberID})
	})
}

func (s *Service) Registrations(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) ([]db.EventRegistrations, error) {
	if _, err := s.Get(ctx, oc, aid); err != nil {
		return nil, err
	}
	return s.q.ListRegistrations(ctx, toPg(aid))
}

// ---------- sweeps (activities:* tasks) ----------

// OverdueActionItems — activities:overdue_action_items (daily): open items
// past due_date flip to overdue; each emits ActionItemOverdue.
// Claim-then-emit lives in ONE tx per row-batch: if an emit fails the
// marks roll back and the next tick retries — a flipped-without-event
// item is worse than a delayed one (Rule 12).
func (s *Service) OverdueActionItems(ctx context.Context) (int, error) {
	var rows []db.MarkOverdueActionItemsRow
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		rows, err = q.MarkOverdueActionItems(ctx)
		if err != nil {
			return err
		}
		for _, r := range rows {
			var tid, oid uuid.UUID
			if a, err := q.GetActivityAny(ctx, r.ActivityID); err == nil {
				oid = fromPg(a.OrganizationID)
				if t, err := q.GetOrgTenantID(ctx, a.OrganizationID); err == nil {
					tid = fromPg(t)
				}
			}
			if err := events.Emit(ctx, q, tid, oid, "ActionItemOverdue",
				"action_item", fromPg(r.ID), uuid.Nil, false,
				map[string]any{"item_id": fromPg(r.ID), "assignee_id": fromPg(r.AssigneeID)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// RemindUpcoming — activities:remind_upcoming (hourly): meetings entering
// their notice window emit MeetingReminderDue for the comms fan-out.
// ClaimReminders marks each row emit-once so the per-tick sweep never
// re-fires a reminder for an already-noticed activity.
func (s *Service) RemindUpcoming(ctx context.Context) (int, error) {
	var rows []db.Activities
	err := s.withTx(ctx, func(q *db.Queries) error {
		var err error
		rows, err = q.ClaimReminders(ctx) // per-org notice_days window
		if err != nil {
			return err
		}
		for _, a := range rows {
			var tenantID uuid.UUID
			if t, err := q.GetOrgTenantID(ctx, a.OrganizationID); err == nil {
				tenantID = fromPg(t)
			}
			if err := events.Emit(ctx, q, tenantID, fromPg(a.OrganizationID), "MeetingReminderDue",
				"activity", fromPg(a.ID), uuid.Nil, false,
				map[string]any{"activity_id": fromPg(a.ID), "starts_at": a.StartsAt.Time}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(rows), nil
}

// RecurrenceIn is the officer-facing template for a recurring activity.
// TimeOfDay ("HH:MM") stamps each instance's start — without it the
// series would inherit whatever hour the daily sweep ran at.
type RecurrenceIn struct {
	Kind        string     `json:"kind"` // meeting|event
	Title       string     `json:"title"`
	Description string     `json:"description"`
	Venue       string     `json:"venue"`
	UnitID      *uuid.UUID `json:"unit_id"`
	TimeOfDay   string     `json:"time_of_day"` // "HH:MM" — required
	DurationMin int        `json:"duration_min"`
	QuorumPct   *int       `json:"quorum_percent"`
	Capacity    *int       `json:"capacity"`
	RRule       string     `json:"rrule"` // FREQ=...;INTERVAL=..;COUNT=..|UNTIL=..
}

// CreateRecurrence registers a series; the daily
// activities:generate_recurrences sweep materializes the instances.
func (s *Service) CreateRecurrence(ctx context.Context, oc *tenant.OrgCtx, in RecurrenceIn) (uuid.UUID, error) {
	if in.Kind != "meeting" && in.Kind != "event" {
		return uuid.Nil, ErrBadKind
	}
	if in.Title == "" || in.RRule == "" {
		return uuid.Nil, ErrBadValue
	}
	if _, err := parseTimeOfDay(in.TimeOfDay); err != nil {
		return uuid.Nil, ErrBadValue
	}
	if len(expandRRule(in.RRule, time.Now(), time.Now(), time.Now().Add(365*24*time.Hour), 0)) == 0 {
		return uuid.Nil, ErrBadValue // grammar must yield at least one instance
	}
	if in.UnitID != nil {
		if _, err := s.q.GetOrgUnit(ctx, db.GetOrgUnitParams{ID: toPg(*in.UnitID), OrganizationID: toPg(oc.OrgID)}); err != nil {
			return uuid.Nil, ErrNotFound
		}
	}
	tpl, err := json.Marshal(map[string]any{
		"title": in.Title, "description": in.Description, "venue": in.Venue,
		"unit_id": in.UnitID, "time_of_day": in.TimeOfDay,
		"duration_min": in.DurationMin, "created_by": oc.MembershipID,
		"quorum_percent": in.QuorumPct, "capacity": in.Capacity,
	})
	if err != nil {
		return uuid.Nil, err
	}
	row, err := s.q.CreateRecurrence(ctx, db.CreateRecurrenceParams{
		OrganizationID: toPg(oc.OrgID), Kind: in.Kind, Template: tpl, Rrule: in.RRule})
	if err != nil {
		return uuid.Nil, err
	}
	return fromPg(row.ID), nil
}

func (s *Service) Recurrences(ctx context.Context, oc *tenant.OrgCtx) ([]db.ActivityRecurrences, error) {
	return s.q.ListRecurrences(ctx, toPg(oc.OrgID))
}

// ActivityInOrg is the Rule-26 seam finance uses to validate
// payments.activity_id — the activities module answers, finance never
// reads the table.
func (s *Service) ActivityInOrg(ctx context.Context, orgID, activityID uuid.UUID) bool {
	_, err := s.q.GetActivity(ctx, db.GetActivityParams{ID: toPg(activityID), OrganizationID: toPg(orgID)})
	return err == nil
}

// ActivitySnapshot is the Rule-26 read seam comms uses for
// MeetingReminderDue payloads — the activities module answers, comms
// never reads the table.
func (s *Service) ActivitySnapshot(ctx context.Context, activityID uuid.UUID) (orgID uuid.UUID, title string, unitID uuid.UUID, startsAt time.Time, ok bool) {
	a, err := s.q.GetActivityAny(ctx, toPg(activityID))
	if err != nil {
		return uuid.Nil, "", uuid.Nil, time.Time{}, false
	}
	return uuid.UUID(a.OrganizationID.Bytes), a.Title,
		uuid.UUID(a.UnitID.Bytes), a.StartsAt.Time, true
}

// parseTimeOfDay validates "HH:MM" and returns the offset into the day.
func parseTimeOfDay(s string) (time.Duration, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute, nil
}

// GenerateRecurrences — activities:generate_recurrences (daily): expands
// RRULE templates into concrete activities over a 35-day horizon.
// Supported grammar: FREQ=DAILY|WEEKLY|MONTHLY|YEARLY, INTERVAL,
// COUNT, UNTIL, BYDAY (WEEKLY), BYMONTHDAY (MONTHLY).
// Instances carry activities.recurrence_id — the (recurrence_id,
// starts_at) unique dedupes generation atomically (CreateRecurrenceInstance
// returns no row on replay, never a duplicate).
func (s *Service) GenerateRecurrences(ctx context.Context, horizonDays int) (int, error) {
	recs, err := s.q.ActiveRecurrences(ctx)
	if err != nil {
		return 0, err
	}
	now := time.Now()
	horizon := now.Add(time.Duration(horizonDays) * 24 * time.Hour)
	created := 0
	for _, rc := range recs {
		recID := fromPg(rc.ID)
		var tpl struct {
			Title       string     `json:"title"`
			Description string     `json:"description"`
			Venue       string     `json:"venue"`
			UnitID      *uuid.UUID `json:"unit_id"`
			TimeOfDay   string     `json:"time_of_day"`
			DurationMin int        `json:"duration_min"`
			CreatedBy   uuid.UUID  `json:"created_by"`
			QuorumPct   *int       `json:"quorum_percent"`
			Capacity    *int       `json:"capacity"`
		}
		if json.Unmarshal(rc.Template, &tpl) != nil || tpl.CreatedBy == uuid.Nil {
			continue
		}
		tod, err := parseTimeOfDay(tpl.TimeOfDay)
		if err != nil {
			tod = 9 * time.Hour // templates predating time_of_day get a sane default
		}
		// COUNT caps the whole series — instances already generated count
		// toward it, so the sweep only mints the remaining budget.
		existing, err := s.q.CountRecurrenceInstances(ctx, rc.ID)
		if err != nil {
			log.Printf("recurrence %s count: %v", rc.ID.Bytes, err)
			continue
		}
		occ := expandRRule(rc.Rrule, rc.CreatedAt.Time, now, horizon, int(existing))
		var tenantID uuid.UUID
		if t, err := s.q.GetOrgTenantID(ctx, rc.OrganizationID); err == nil {
			tenantID = fromPg(t)
		}
		for _, at := range occ {
			at = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, at.Location()).Add(tod)
			err = s.withTx(ctx, func(q *db.Queries) error {
				var ends pgtype.Timestamptz
				if tpl.DurationMin > 0 {
					ends = pgTs(at.Add(time.Duration(tpl.DurationMin) * time.Minute))
				}
				var unit pgtype.UUID
				if tpl.UnitID != nil {
					unit = toPg(*tpl.UnitID)
				}
				var qp, cap pgtype.Int4
				if tpl.QuorumPct != nil {
					qp = pgtype.Int4{Int32: int32(*tpl.QuorumPct), Valid: true}
				}
				if tpl.Capacity != nil {
					cap = pgtype.Int4{Int32: int32(*tpl.Capacity), Valid: true}
				}
				row, err := q.CreateRecurrenceInstance(ctx, db.CreateRecurrenceInstanceParams{
					OrganizationID: rc.OrganizationID, Kind: rc.Kind, Title: tpl.Title,
					Description: pgText(tpl.Description),
					Venue:       pgText(tpl.Venue), StartsAt: pgTs(at), EndsAt: ends,
					UnitID: unit, QuorumPercent: qp, Capacity: cap,
					CreatedBy: toPg(tpl.CreatedBy), RecurrenceID: rc.ID})
				if err == pgx.ErrNoRows {
					return nil // instance already generated — atomic dedupe
				}
				if err != nil {
					return err
				}
				created++
				return events.Emit(ctx, q, tenantID, fromPg(rc.OrganizationID), "RecurrenceInstanceGenerated",
					"activity", fromPg(row.ID), uuid.Nil, false,
					map[string]any{"activity_id": fromPg(row.ID), "recurrence_id": recID})
			})
			if err != nil {
				log.Printf("recurrence %s instance %s: %v", rc.ID.Bytes, at, err)
			}
		}
	}
	return created, nil
}

// expandRRule evaluates the supported RRULE grammar by scanning
// day-by-day from `after` to `until` (bounded by the horizon):
// FREQ=DAILY|WEEKLY|MONTHLY|YEARLY, INTERVAL=n, COUNT=n, UNTIL=date,
// BYDAY=MO,TU,.. (WEEKLY — default: the anchor weekday),
// BYMONTHDAY=n[,n] (MONTHLY — default: the anchor day).
// `existing` is the count of instances already generated — COUNT caps the
// series total, so generation stops when existing+new reaches it.
// Anything outside the grammar yields no instances rather than a
// wrong guess.
func expandRRule(rrule string, anchor, after, horizon time.Time, existing int) []time.Time {
	var freq, bydayStr, bymondayStr string
	interval, count := 1, 0
	until := horizon
	for _, part := range strings.Split(rrule, ";") {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch kv[0] {
		case "FREQ":
			freq = kv[1]
		case "INTERVAL":
			if n, err := strconv.Atoi(kv[1]); err == nil && n > 0 {
				interval = n
			}
		case "COUNT":
			if n, err := strconv.Atoi(kv[1]); err == nil {
				count = n
			}
		case "UNTIL":
			if t, err := time.Parse("20060102", strings.TrimSuffix(kv[1], "Z")); err == nil {
				until = t.Add(24*time.Hour - time.Second) // UNTIL is inclusive
			} else if t, err := time.Parse(time.RFC3339, kv[1]); err == nil {
				until = t
			}
		case "BYDAY":
			bydayStr = kv[1]
		case "BYMONTHDAY":
			bymondayStr = kv[1]
		}
	}
	if until.After(horizon) {
		until = horizon
	}
	anchor = time.Date(anchor.Year(), anchor.Month(), anchor.Day(), 0, 0, 0, 0, anchor.Location())

	var byday map[time.Weekday]bool
	if bydayStr != "" {
		byday = map[time.Weekday]bool{}
		codes := map[string]time.Weekday{"SU": 0, "MO": 1, "TU": 2, "WE": 3, "TH": 4, "FR": 5, "SA": 6}
		for _, d := range strings.Split(bydayStr, ",") {
			wd, ok := codes[d]
			if !ok {
				return nil // unknown BYDAY token — fail closed
			}
			byday[wd] = true
		}
	}
	var bymday map[int]bool
	if bymondayStr != "" {
		bymday = map[int]bool{}
		for _, d := range strings.Split(bymondayStr, ",") {
			n, err := strconv.Atoi(d)
			if err != nil || n < 1 || n > 31 {
				return nil
			}
			bymday[n] = true
		}
	}
	monthsBetween := func(a, b time.Time) int {
		return (b.Year()-a.Year())*12 + int(b.Month()) - int(a.Month())
	}
	matches := func(day time.Time) bool {
		switch freq {
		case "DAILY":
			return byday == nil && bymday == nil && int(day.Sub(anchor).Hours()/24)%interval == 0
		case "WEEKLY":
			if bymday != nil {
				return false
			}
			wk := int(day.Sub(anchor).Hours() / (24 * 7))
			if byday == nil {
				return day.Weekday() == anchor.Weekday() && wk%interval == 0
			}
			return byday[day.Weekday()] && wk%interval == 0
		case "MONTHLY":
			if byday != nil {
				return false
			}
			if bymday == nil {
				return day.Day() == anchor.Day() && monthsBetween(anchor, day)%interval == 0
			}
			return bymday[day.Day()] && monthsBetween(anchor, day)%interval == 0
		case "YEARLY":
			return byday == nil && bymday == nil &&
				day.Month() == anchor.Month() && day.Day() == anchor.Day() &&
				(day.Year()-anchor.Year())%interval == 0
		}
		return false
	}
	var out []time.Time
	// Scan from the day after the anchor — or from `after` when the series
	// is already running: missed occurrences in the past are never
	// minted retroactively, only upcoming ones within the horizon.
	start := anchor.AddDate(0, 0, 1)
	if a := time.Date(after.Year(), after.Month(), after.Day(), 0, 0, 0, 0, after.Location()); a.After(start) {
		start = a
	}
	for day := start; !day.After(until); day = day.AddDate(0, 0, 1) {
		if !matches(day) {
			continue
		}
		out = append(out, day)
		if count > 0 && existing+len(out) >= count {
			break // series total reached — not just this window's
		}
	}
	return out
}

// OpenActionItemsForOrg — §16 handover section: every still-open item
// across the org's activities (the governance module consumes this via
// its handover-section seam, Rule 26).
func (s *Service) OpenActionItemsForOrg(ctx context.Context, oc *tenant.OrgCtx) ([]db.ActionItems, error) {
	return s.q.OpenActionItemsForOrg(ctx, toPg(oc.OrgID))
}
