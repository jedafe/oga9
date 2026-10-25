// Ref: oga9-engineering/sections/11-members.md + adr/ADR-014
// Members: applications + screening, the ADR-014 lifecycle state machine,
// per-org member_no sequence, sponsors, renewals, cards, imports and the
// computed eligibility surface. Every transition emits its catalogued
// event in the same transaction (Rule 12) and invalidates the member's
// permission/org-context caches (Rule 6).
package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
)

var (
	ErrInvalidTransition   = errors.New("invalid_state_transition")
	ErrSponsorRequired     = errors.New("sponsor_required")
	ErrApplicationNotFound = errors.New("application_not_found")
	ErrAlreadyApplied      = errors.New("application_exists")
	ErrDuplicateRenewal    = errors.New("renewal_exists")
	ErrImportNotFound      = errors.New("import_not_found")
)

// ADR-014 state machine: forward pipeline, suspension, terminal states,
// life-member elevation. Application statuses share the same vocabulary —
// intake decisions (screen/approve/reject/withdraw) move the APPLICATION
// row; approved applications materialize a membership at 'approved'.
var memberTransitions = map[string][]string{
	"invited":    {"applied"},
	"applied":    {"screened", "rejected", "withdrawn"},
	"screened":   {"approved", "rejected", "withdrawn"},
	"approved":   {"registered"},
	"registered": {"active"},
	"active":     {"suspended", "expelled", "resigned", "deceased", "life_member"},
	"suspended":  {"active", "expelled", "resigned", "deceased"},
}

// API action verbs → target states. Route bodies carry verbs, not state
// names — keeps the client contract stable if the machine grows.
var memberActions = map[string]string{
	"screen": "screened", "approve": "approved", "reject": "rejected", "withdraw": "withdrawn",
	"register": "registered", "activate": "active",
	"suspend": "suspended", "reinstate": "active", "expel": "expelled",
	"resign": "resigned", "deceased": "deceased", "life": "life_member",
}

// transitionEvent maps (from,to) onto the §11/catalog event name —
// suspended→active is MembershipReinstated, everything else keys on `to`.
var transitionEvents = map[string]string{
	"applied": "MemberApplied", "screened": "MemberScreened", "approved": "MemberApproved",
	"rejected": "MemberRejected", "registered": "MemberRegistered", "active": "MembershipActivated",
	"suspended": "MembershipSuspended", "expelled": "MemberExpelled", "resigned": "MemberResigned",
	"deceased": "MemberDeceased", "life_member": "LifeMemberAwarded",
}

func transitionEvent(from, to string) string {
	if to == "active" && from == "suspended" {
		return "MembershipReinstated"
	}
	return transitionEvents[to]
}

// terminalState is where the membership ends — appointments vacate,
// committee seats clear, cards revoke (§11 business rules).
func terminalState(s string) bool {
	return s == "expelled" || s == "resigned" || s == "deceased"
}

// memberNo mints the next per-org member number. The format comes from the
// org.membership.number_format setting ({SLUG}-{YYYY}-{seq5} default);
// the sequence itself is atomic (NextMemberNoSeq) and never reused.
func (s *Service) memberNo(ctx context.Context, q *db.Queries, orgID uuid.UUID) (string, error) {
	slug, err := q.GetOrgSlug(ctx, toPg(orgID))
	if err != nil {
		return "", err
	}
	seq, err := q.NextMemberNoSeq(ctx, toPg(orgID))
	if err != nil {
		return "", err
	}
	format := "{SLUG}-{YYYY}-{seq5}"
	if raw, err := q.GetOrgSetting(ctx, db.GetOrgSettingParams{
		OrganizationID: toPg(orgID), Key: "org.membership.number_format",
	}); err == nil && len(raw) > 0 {
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			format = v
		}
	}
	no := strings.NewReplacer(
		"{SLUG}", strings.ToUpper(slug),
		"{YYYY}", fmt.Sprintf("%d", time.Now().Year()),
		"{seq5}", fmt.Sprintf("%05d", seq),
	).Replace(format)
	return no, nil
}

// Apply files a membership application. Public pre-signup applications
// pass userID=uuid.Nil (email carries identity until they register).
// sponsor_required comes from the org setting (0|1|2) — a sponsor must be
// an active member of the same org.
func (s *Service) Apply(ctx context.Context, orgID, userID uuid.UUID, email, phone string, answers json.RawMessage, sponsorID uuid.UUID) (uuid.UUID, error) {
	if email == "" && phone == "" {
		return uuid.Nil, ErrInviteNoContact
	}
	required := 0
	if raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{
		OrganizationID: toPg(orgID), Key: "org.membership.sponsor_required",
	}); err == nil && len(raw) > 0 {
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			required = int(v)
		}
	}
	if required > 0 && sponsorID == uuid.Nil {
		return uuid.Nil, ErrSponsorRequired
	}
	if userID != uuid.Nil {
		if n, err := s.q.GetApplicationCountForUser(ctx, db.GetApplicationCountForUserParams{OrganizationID: toPg(orgID), UserID: toPg(userID)}); err != nil {
			return uuid.Nil, err
		} else if n > 0 {
			return uuid.Nil, ErrAlreadyApplied
		}
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		if sponsorID != uuid.Nil {
			sp, err := q.GetMembershipByID(ctx, toPg(sponsorID))
			if err != nil || fromPg(sp.OrganizationID) != orgID || sp.LifecycleState != "active" {
				return ErrNotMember // sponsor must be an active member of this org
			}
		}
		r, err := q.CreateApplication(ctx, db.CreateApplicationParams{
			OrganizationID: toPg(orgID), UserID: pgUUIDPtr(userID),
			Email: pgText(email), Phone: pgText(phone),
			Answers:         answers,
			SponsorMemberID: pgUUIDPtr(sponsorID),
		})
		if err != nil {
			return err
		}
		id = fromPg(r)
		return events.Emit(ctx, q, uuid.Nil, orgID, "MemberApplied", "membership_application", id, userID, true,
			map[string]any{"application_id": id, "org_id": orgID, "user_id": userID, "email": email})
	})
	return id, err
}

// PublicApply resolves the org by slug (listed directory orgs only) for
// the unauthenticated apply path.
func (s *Service) PublicApply(ctx context.Context, slug, email, phone string, answers json.RawMessage) (uuid.UUID, error) {
	org, err := s.q.GetPublicOrgBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, ErrOrgNotFound
	}
	if err != nil {
		return uuid.Nil, err
	}
	return s.Apply(ctx, fromPg(org.ID), uuid.Nil, email, phone, answers, uuid.Nil)
}

// Applications lists the screening queue (applied|screened|approved|rejected|withdrawn).
func (s *Service) Applications(ctx context.Context, oc *OrgCtx, status string) ([]db.ListApplicationsRow, error) {
	if status == "" {
		status = "applied"
	}
	return s.q.ListApplications(ctx, db.ListApplicationsParams{OrgID: toPg(oc.OrgID), Status: status})
}

// DecideApplication moves the intake state machine. On approve, the same
// tx materializes the membership (state 'approved'), mints member_no,
// records the sponsor link, and emits MemberApproved + SponsorshipRecorded.
// Pre-signup applications (user_id NULL) approve without a membership —
// the membership materializes when they register and accept (Phase-02 hook).
func (s *Service) DecideApplication(ctx context.Context, oc *OrgCtx, appID uuid.UUID, action string) error {
	to := memberActions[action]
	if to == "" || to == "registered" {
		return ErrInvalidTransition
	}
	app, err := s.q.GetApplication(ctx, toPg(appID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrApplicationNotFound
	}
	if err != nil {
		return err
	}
	if fromPg(app.OrganizationID) != oc.OrgID {
		return ErrApplicationNotFound
	}
	var from []string
	for s, targets := range memberTransitions {
		for _, t := range targets {
			if t == to {
				from = append(from, s)
			}
		}
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.SetApplicationStatus(ctx, db.SetApplicationStatusParams{
			ID: toPg(appID), Column2: from, Status: to, ScreenedBy: pgUUIDPtr(oc.MembershipID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition // concurrent decision or stale state
		}
		if err != nil {
			return err
		}
		if err := events.Emit(ctx, q, uuid.Nil, oc.OrgID, transitionEvent(app.Status, to), "membership_application", appID, oc.UserID, true,
			map[string]any{"application_id": appID, "org_id": oc.OrgID, "to": to}); err != nil {
			return err
		}
		if to != "approved" || !app.UserID.Valid {
			return nil
		}
		// approved: materialize the membership pipeline row + member_no.
		no, err := s.memberNo(ctx, q, oc.OrgID)
		if err != nil {
			return err
		}
		mid, err := q.CreateMembership(ctx, db.CreateMembershipParams{
			OrganizationID: toPg(oc.OrgID), UserID: app.UserID,
			MemberNo: pgText(no), Category: "floor", LifecycleState: "approved",
		})
		if err != nil {
			return err
		}
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MemberApproved", "organization_membership", fromPg(mid), oc.UserID, true,
			map[string]any{"membership_id": fromPg(mid), "org_id": oc.OrgID, "member_no": no, "application_id": appID}); err != nil {
			return err
		}
		if app.SponsorMemberID.Valid {
			if err := q.AddSponsor(ctx, db.AddSponsorParams{
				MembershipID: mid, SponsorMemberID: app.SponsorMemberID, Kind: "sponsor",
			}); err != nil {
				return err
			}
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "SponsorshipRecorded", "organization_membership", fromPg(mid), oc.UserID, true,
				map[string]any{"membership_id": fromPg(mid), "sponsor_member_id": fromPg(app.SponsorMemberID), "kind": "sponsor"})
		}
		_ = row
		return nil
	})
}

// TransitionMember drives the post-approval lifecycle machine. Terminal
// transitions (expelled/resigned/deceased) vacate appointments + committee
// seats + cards in the same tx; every legal move emits its event and
// invalidates the member's perm/org-context caches (Rule 6).
func (s *Service) TransitionMember(ctx context.Context, oc *OrgCtx, memberID uuid.UUID, action string) error {
	to := memberActions[action]
	if to == "" || action == "screen" || action == "approve" || action == "reject" || action == "withdraw" {
		return ErrInvalidTransition
	}
	m, err := s.q.GetMembershipFull(ctx, toPg(memberID))
	if errors.Is(err, pgx.ErrNoRows) || fromPg(m.OrganizationID) != oc.OrgID {
		return ErrNotMember
	}
	if err != nil {
		return err
	}
	from := m.LifecycleState
	legal := false
	for _, t := range memberTransitions[from] {
		if t == to {
			legal = true
		}
	}
	// reinstate resolves through the shared from→to map: suspended→active.
	if !legal {
		return ErrInvalidTransition
	}
	userID := fromPg(m.UserID)
	err = s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.TransitionMembership(ctx, db.TransitionMembershipParams{
			ID: toPg(memberID), OrganizationID: toPg(oc.OrgID), LifecycleState: to, Column4: []string{from},
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition // raced with another transition
		} else if err != nil {
			return err
		}
		if to == "active" {
			// activation is "joining" — the member role lands here (invite
			// path grants it on accept; the application path grants it now).
			// ON CONFLICT DO NOTHING keeps reinstate idempotent.
			if err := q.AssignRoleBySlug(ctx, db.AssignRoleBySlugParams{
				UserID: m.UserID, TenantID: toPg(oc.TenantID), Slug: "member",
			}); err != nil {
				return err
			}
		}
		if terminalState(to) {
			rows, err := q.EndMemberAppointments(ctx, toPg(memberID))
			if err != nil {
				return err
			}
			for _, a := range rows {
				if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AppointmentEnded", "officer_appointment", fromPg(a.ID), oc.UserID, true,
					map[string]any{"appointment_id": fromPg(a.ID), "term_id": fromPg(a.TermID)}); err != nil {
					return err
				}
			}
			if _, err := q.RemoveMemberCommitteeSeats(ctx, toPg(memberID)); err != nil {
				return err
			}
			if err := q.RevokeCards(ctx, toPg(memberID)); err != nil {
				return err
			}
		}
		// TODO: MemberDeceased → welfare bereavement-case candidate flags
		// when §13 welfare lands (sourced event already carries the hook).
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, transitionEvent(from, to), "organization_membership", memberID, oc.UserID, true,
			map[string]any{"membership_id": memberID, "org_id": oc.OrgID, "user_id": userID, "from": from, "to": to})
	})
	if err != nil {
		return err
	}
	// Rule 6: membership-state changes flip the resolved grant set — the
	// resolver filters suspended/terminal states, so caches must drop.
	s.InvalidatePermCache(ctx, userID, oc.OrgID)
	s.InvalidateOrgCtx(ctx, userID, oc.OrgID)
	return nil
}

// Members lists the directory with optional category/state filters.
func (s *Service) Members(ctx context.Context, oc *OrgCtx, category, state string) ([]db.ListMembersRow, error) {
	return s.q.ListMembers(ctx, db.ListMembersParams{OrgID: toPg(oc.OrgID), Category: category, State: state})
}

func (s *Service) MemberDetail(ctx context.Context, oc *OrgCtx, memberID uuid.UUID) (db.GetMembershipFullRow, error) {
	m, err := s.q.GetMembershipFull(ctx, toPg(memberID))
	if errors.Is(err, pgx.ErrNoRows) || fromPg(m.OrganizationID) != oc.OrgID {
		return db.GetMembershipFullRow{}, ErrNotMember
	}
	return m, err
}

// PatchMember is the limited admin edit surface — category only; lifecycle
// never changes here (transitions are explicit).
func (s *Service) PatchMember(ctx context.Context, oc *OrgCtx, memberID uuid.UUID, category string) error {
	if !memberCategories[category] {
		return ErrBadCategory
	}
	if _, err := s.q.PatchMembership(ctx, db.PatchMembershipParams{
		ID: toPg(memberID), OrgID: toPg(oc.OrgID), Category: pgText(category),
	}); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotMember
	} else {
		return err
	}
}

// RenewMember records a dues period (membership_renewals is UNIQUE per
// membership+period — re-attempts 409, never double-record). Renewal marks
// the member in good standing again once §12 finance wires payment_id.
func (s *Service) RenewMember(ctx context.Context, oc *OrgCtx, memberID uuid.UUID, period string, startsAt, endsAt time.Time) (uuid.UUID, error) {
	m, err := s.MemberDetail(ctx, oc, memberID)
	if err != nil {
		return uuid.Nil, err
	}
	if m.LifecycleState != "active" && m.LifecycleState != "suspended" {
		return uuid.Nil, ErrInvalidTransition
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		r, err := q.CreateRenewal(ctx, db.CreateRenewalParams{
			MembershipID: toPg(memberID), PeriodLabel: period,
			StartsAt: pgDate(startsAt), EndsAt: pgDate(endsAt),
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrDuplicateRenewal
			}
			return err
		}
		id = fromPg(r)
		if err := q.TouchRenewal(ctx, toPg(memberID)); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MembershipRenewed", "membership_renewal", id, oc.UserID, true,
			map[string]any{"renewal_id": id, "membership_id": memberID, "org_id": oc.OrgID, "period_label": period})
	})
	return id, err
}

// IssueCard mints a member card — serial is unique org-wide (member_no +
// suffix); physical revoke happens on terminal transitions.
func (s *Service) IssueCard(ctx context.Context, oc *OrgCtx, memberID uuid.UUID, design json.RawMessage) (db.IssueCardRow, error) {
	m, err := s.MemberDetail(ctx, oc, memberID)
	if err != nil {
		return db.IssueCardRow{}, err
	}
	if m.LifecycleState != "active" && m.LifecycleState != "life_member" {
		return db.IssueCardRow{}, ErrInvalidTransition // cards issue for settled members only
	}
	if len(design) == 0 {
		design = json.RawMessage("{}")
	}
	serial := fmt.Sprintf("CARD-%s-%s", strings.ToUpper(uuid.NewString()[:8]), strings.ToUpper(uuid.NewString()[:4]))
	if m.MemberNo.Valid && m.MemberNo.String != "" {
		serial = fmt.Sprintf("%s-%s", m.MemberNo.String, strings.ToUpper(uuid.NewString()[:8]))
	}
	var card db.IssueCardRow
	err = s.withTx(ctx, func(q *db.Queries) error {
		r, err := q.IssueCard(ctx, db.IssueCardParams{
			MembershipID: toPg(memberID), Serial: serial, Design: design,
		})
		if err != nil {
			return err
		}
		card = r
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CardIssued", "membership_card", fromPg(r.ID), oc.UserID, true,
			map[string]any{"card_id": fromPg(r.ID), "membership_id": memberID, "serial": serial})
	})
	return card, err
}

// MemberHistory returns the member's sourced event stream (ADR-014:
// complete replayable journey — application + lifecycle + governance events).
func (s *Service) MemberHistory(ctx context.Context, oc *OrgCtx, memberID uuid.UUID) ([]db.MemberHistoryRow, error) {
	m, err := s.MemberDetail(ctx, oc, memberID)
	if err != nil {
		return nil, err
	}
	return s.q.MemberHistory(ctx, db.MemberHistoryParams{
		OrgID: toPg(oc.OrgID), MembershipID: toPg(memberID), UserID: m.UserID,
	})
}

// MyMemberships is the /me/memberships surface — all memberships across orgs.
func (s *Service) MyMemberships(ctx context.Context, userID uuid.UUID) ([]db.ListUserMembershipsRow, error) {
	return s.q.ListUserMemberships(ctx, toPg(userID))
}

// Eligible is the computed voting/office/welfare surface (§11: eligibility
// is never a stored flag) — active or life_member + good dues standing.
// Category + tenure rules land with the rules engine (Rule 23).
func Eligible(state, duesStanding string) bool {
	return (state == "active" || state == "life_member") && duesStanding == "good"
}

// CreateImport stores a parsed import payload and lets the handler enqueue
// membership:import — the row is the durable work record (Rule 8 fan-out).
func (s *Service) CreateImport(ctx context.Context, oc *OrgCtx, filename string, rows json.RawMessage) (uuid.UUID, error) {
	id, err := s.q.CreateImport(ctx, db.CreateImportParams{
		OrganizationID: toPg(oc.OrgID), UploadedBy: toPg(oc.MembershipID),
		Filename: filename, Data: rows,
	})
	return fromPg(id), err
}

// ProcessImport turns a member_imports row into applications — each parsed
// row becomes an 'applied'-state application (§11: imports never create
// direct members). Per-row failures collect into errors, never abort the batch.
func (s *Service) ProcessImport(ctx context.Context, importID uuid.UUID) error {
	imp, err := s.q.GetImport(ctx, toPg(importID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrImportNotFound
	}
	if err != nil {
		return err
	}
	orgID := fromPg(imp.OrganizationID)
	var rows []map[string]any
	if err := json.Unmarshal(imp.Data, &rows); err != nil {
		return s.q.FinishImport(ctx, db.FinishImportParams{
			ID: toPg(importID), Status: "failed", Errors: mustJSON(map[string]any{"parse": err.Error()}),
		})
	}
	ok, errs := 0, []any{}
	for i, row := range rows {
		email, _ := row["email"].(string)
		phone, _ := row["phone"].(string)
		answers, _ := json.Marshal(row)
		if email == "" && phone == "" {
			errs = append(errs, map[string]any{"row": i, "error": "email_or_phone_required"})
			continue
		}
		err := s.withTx(ctx, func(q *db.Queries) error {
			r, err := q.CreateApplication(ctx, db.CreateApplicationParams{
				OrganizationID: imp.OrganizationID, Email: pgText(email), Phone: pgText(phone),
				Answers: answers,
			})
			if err != nil {
				return err
			}
			return events.Emit(ctx, q, uuid.Nil, orgID, "MemberImported", "membership_application", fromPg(r), uuid.Nil, true,
				map[string]any{"application_id": fromPg(r), "org_id": orgID, "import_id": importID, "row": i})
		})
		if err != nil {
			errs = append(errs, map[string]any{"row": i, "error": err.Error()})
			continue
		}
		ok++
	}
	return s.q.FinishImport(ctx, db.FinishImportParams{
		ID: toPg(importID), Status: "done",
		RowCount: pgtype.Int4{Int32: int32(len(rows)), Valid: true}, OkCount: pgtype.Int4{Int32: int32(ok), Valid: true}, ErrCount: pgtype.Int4{Int32: int32(len(errs)), Valid: true},
		Errors: mustJSON(errs),
	})
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
