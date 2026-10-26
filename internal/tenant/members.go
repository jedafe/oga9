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
	"log"
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
	ErrAlreadyMember       = errors.New("already_a_member")
	ErrDuplicateRenewal    = errors.New("renewal_exists")
	ErrImportNotFound      = errors.New("import_not_found")
	ErrRegistrationPending = errors.New("registration_unpaid")
	ErrMemberLimitExceeded = errors.New("limit_exceeded")
)

// ADR-014 state machine: forward pipeline, suspension, terminal states,
// life-member elevation. Application statuses share the same vocabulary —
// intake decisions (screen/approve/reject/withdraw) move the APPLICATION
// row; approved applications materialize a membership at 'approved'.
var memberTransitions = map[string][]string{
	"invited":    {"applied", "withdrawn"}, // ADR-014: any → withdrawn while pre-active
	"applied":    {"screened", "rejected", "withdrawn"},
	"screened":   {"approved", "rejected", "withdrawn"},
	"approved":   {"registered", "withdrawn"}, // §11: any → withdrawn while pre-active
	"registered": {"active", "withdrawn"},
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
	"rejected": "MemberRejected", "withdrawn": "MemberWithdrawn", "registered": "MemberRegistered",
	"active":    "MembershipActivated",
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

// Apply files a membership application. sponsorIDs[0] is the primary
// sponsor (sponsor_member_id column); extras stage as application-linked
// member_sponsors rows and migrate to the membership on approve —
// satisfying sponsor_required up to 2. Every sponsor must be an active
// member of the same org. sponsor_required is enforced at APPROVE (§11
// test scenario) — intake stays open so a screener can attach sponsors.
func (s *Service) Apply(ctx context.Context, orgID, userID uuid.UUID, email, phone string, answers json.RawMessage, sponsorIDs []uuid.UUID) (uuid.UUID, error) {
	if email == "" && phone == "" {
		return uuid.Nil, ErrInviteNoContact
	}
	if len(answers) == 0 {
		answers = json.RawMessage("{}")
	}
	if len(sponsorIDs) > 2 {
		return uuid.Nil, ErrBadValue // catalog caps sponsor_required at 2
	}
	if userID != uuid.Nil {
		// A live membership makes a new application meaningless — approve
		// would hit UNIQUE(organization_id, user_id) mid-tx.
		if _, err := s.q.GetMembership(ctx, db.GetMembershipParams{
			OrganizationID: toPg(orgID), UserID: toPg(userID),
		}); err == nil {
			return uuid.Nil, ErrAlreadyMember
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, err
		}
		if n, err := s.q.GetApplicationCountForUser(ctx, db.GetApplicationCountForUserParams{OrganizationID: toPg(orgID), UserID: toPg(userID)}); err != nil {
			return uuid.Nil, err
		} else if n > 0 {
			return uuid.Nil, ErrAlreadyApplied
		}
	} else {
		if email != "" {
			if exists, err := s.q.ExistsOrgMemberByContact(ctx, db.ExistsOrgMemberByContactParams{
				OrgID: toPg(orgID), Email: email,
			}); err != nil {
				return uuid.Nil, err
			} else if exists {
				return uuid.Nil, ErrAlreadyMember // public intake can't bypass an existing membership
			}
		}
		if exists, err := s.q.ExistsOpenApplicationByEmail(ctx, db.ExistsOpenApplicationByEmailParams{
			OrgID: toPg(orgID), Email: email, Phone: phone,
		}); err != nil {
			return uuid.Nil, err
		} else if exists {
			return uuid.Nil, ErrAlreadyApplied // public intake dedupes on contact
		}
	}
	var primary pgtype.UUID
	if len(sponsorIDs) > 0 {
		primary = toPg(sponsorIDs[0])
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		for _, sid := range sponsorIDs {
			sp, err := q.GetMembershipByID(ctx, toPg(sid))
			if err != nil || fromPg(sp.OrganizationID) != orgID || sp.LifecycleState != "active" {
				return ErrNotMember // sponsors must be active members of this org
			}
		}
		r, err := q.CreateApplication(ctx, db.CreateApplicationParams{
			OrganizationID: toPg(orgID), UserID: pgUUIDPtr(userID),
			Email: pgText(email), Phone: pgText(phone),
			Answers:         answers,
			SponsorMemberID: primary,
		})
		if err != nil {
			return err
		}
		id = fromPg(r)
		for _, sid := range sponsorIDs[min(len(sponsorIDs), 1):] {
			if sid == sponsorIDs[0] {
				continue // extra duplicating the primary — not a second sponsor
			}
			if err := q.AddApplicationSponsor(ctx, db.AddApplicationSponsorParams{
				ApplicationID: r, SponsorMemberID: toPg(sid), Kind: "sponsor",
			}); err != nil {
				return err
			}
		}
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
	return s.Apply(ctx, fromPg(org.ID), uuid.Nil, email, phone, answers, nil)
}

// PublicApplyForm feeds GET /directory/orgs/{slug}/apply — the org's
// name + configurable app_fields schema + sponsor requirement so the
// public form renders before anyone submits.
func (s *Service) PublicApplyForm(ctx context.Context, slug string) (map[string]any, error) {
	org, err := s.q.GetPublicOrgBySlug(ctx, slug)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrOrgNotFound
	}
	if err != nil {
		return nil, err
	}
	fields := json.RawMessage("[]")
	if raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{
		OrganizationID: org.ID, Key: "org.membership.app_fields",
	}); err == nil && len(raw) > 0 {
		fields = raw
	}
	sponsorRequired := 0
	if raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{
		OrganizationID: org.ID, Key: "org.membership.sponsor_required",
	}); err == nil && len(raw) > 0 {
		var v float64
		if json.Unmarshal(raw, &v) == nil {
			sponsorRequired = int(v)
		}
	}
	return map[string]any{
		"org_name": org.Name, "app_fields": fields, "sponsor_required": sponsorRequired,
	}, nil
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
// extraSponsors lets a screener attach missing sponsors at decision time —
// public/pre-signup applicants can't supply sponsor ids at intake, so a
// sponsor-required org's queue would otherwise deadlock at approve.
func (s *Service) DecideApplication(ctx context.Context, oc *OrgCtx, appID uuid.UUID, action string, extraSponsors []uuid.UUID) error {
	// Decision verbs per the intake machine: screen|approve|reject|withdraw.
	// 'withdraw' is officer-on-behalf cleanup (an applicant self-withdraws
	// via the /withdraw route); 'register' stays a membership transition.
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
		// Screener-attached sponsors only make sense for intake-forward
		// actions; dedupe against the primary and each other so the
		// sponsor_required numerator can't count one person twice.
		if action == "screen" || action == "approve" {
			seen := map[uuid.UUID]bool{}
			for _, sid := range extraSponsors {
				if seen[sid] || (app.SponsorMemberID.Valid && fromPg(app.SponsorMemberID) == sid) {
					continue
				}
				seen[sid] = true
				sp, err := q.GetMembershipByID(ctx, toPg(sid))
				if err != nil || fromPg(sp.OrganizationID) != oc.OrgID || sp.LifecycleState != "active" {
					return ErrNotMember
				}
				if err := q.AddApplicationSponsor(ctx, db.AddApplicationSponsorParams{
					ApplicationID: toPg(appID), SponsorMemberID: toPg(sid), Kind: "sponsor",
				}); err != nil {
					return err
				}
			}
		}
		if to == "approved" {
			// §11 test: approve enforces org.membership.sponsor_required —
			// total sponsors (primary + staged) must reach the org's count.
			if raw, err := q.GetOrgSetting(ctx, db.GetOrgSettingParams{
				OrganizationID: toPg(oc.OrgID), Key: "org.membership.sponsor_required",
			}); err == nil && len(raw) > 0 {
				var required float64
				if json.Unmarshal(raw, &required) == nil && required > 0 {
					if n, err := q.CountApplicationSponsors(ctx, toPg(appID)); err != nil {
						return err
					} else if int64(n) < int64(required) {
						return ErrSponsorRequired
					}
				}
			}
		}
		_, err = q.SetApplicationStatus(ctx, db.SetApplicationStatusParams{
			ID: toPg(appID), Column2: from, Status: to, ScreenedBy: pgUUIDPtr(oc.MembershipID),
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition // concurrent decision or stale state
		}
		if err != nil {
			return err
		}
		// App-level decision event — skipped when the name isn't catalogued
		// (withdrawn has no domain event) or when approve emits the
		// canonical membership-level MemberApproved below.
		if name := transitionEvent(app.Status, to); name != "" && to != "approved" {
			if err := events.Emit(ctx, q, uuid.Nil, oc.OrgID, name, "membership_application", appID, oc.UserID, true,
				map[string]any{"application_id": appID, "org_id": oc.OrgID, "to": to}); err != nil {
				return err
			}
		}
		if to != "approved" || !app.UserID.Valid {
			// Pre-signup approval still records the decision event —
			// the membership materializes when they register
			// (MaterializeApprovedApplications, the verify/login hook).
			if to == "approved" && !app.UserID.Valid {
				return events.Emit(ctx, q, uuid.Nil, oc.OrgID, "MemberApproved", "membership_application", appID, oc.UserID, true,
					map[string]any{"application_id": appID, "org_id": oc.OrgID, "to": to})
			}
			return nil
		}
		return s.materializeMembership(ctx, q, oc.TenantID, oc.OrgID, appID, fromPg(app.UserID), app.SponsorMemberID, oc.UserID)
	})
}

// materializeMembership creates the pipeline membership for an approved
// application: mints member_no, lands the primary + staged sponsors, and
// emits the membership-level MemberApproved. Shared by DecideApplication
// (approve of a user-linked application) and the registration hook
// (pre-signup applications materializing on first verified bind).
func (s *Service) materializeMembership(ctx context.Context, q *db.Queries, tenantID, orgID, appID, userID uuid.UUID, primarySponsor pgtype.UUID, actor uuid.UUID) error {
	no, err := s.memberNo(ctx, q, orgID)
	if err != nil {
		return err
	}
	mid, err := q.CreateMembership(ctx, db.CreateMembershipParams{
		OrganizationID: toPg(orgID), UserID: toPg(userID),
		MemberNo: pgText(no), Category: "floor", LifecycleState: "approved",
	})
	if err != nil {
		return err
	}
	if err := events.Emit(ctx, q, tenantID, orgID, "MemberApproved", "organization_membership", fromPg(mid), actor, true,
		map[string]any{"membership_id": fromPg(mid), "org_id": orgID, "member_no": no, "application_id": appID}); err != nil {
		return err
	}
	if primarySponsor.Valid {
		if err := q.AddSponsor(ctx, db.AddSponsorParams{
			MembershipID: mid, SponsorMemberID: primarySponsor, Kind: "sponsor",
		}); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, tenantID, orgID, "SponsorshipRecorded", "organization_membership", fromPg(mid), actor, true,
			map[string]any{"membership_id": fromPg(mid), "sponsor_member_id": fromPg(primarySponsor), "kind": "sponsor"}); err != nil {
			return err
		}
	}
	// staged extras become membership sponsor rows too (copy+delete is
	// unique-index-safe vs an in-place UPDATE when an extra duplicates
	// the primary).
	if err := q.CopyApplicationSponsors(ctx, db.CopyApplicationSponsorsParams{
		MembershipID: mid, ApplicationID: toPg(appID),
	}); err != nil {
		return err
	}
	return q.DeleteApplicationSponsors(ctx, toPg(appID))
}

// MaterializeApprovedApplications is the §11 registration hook: pre-signup
// applications approved while the applicant had no account become real
// memberships once their verified email binds a user. Called from the
// identity surface after verify/login — idempotent: linking the
// application's user_id is the consume marker, and a second binder loses
// the CAS and skips.
func (s *Service) MaterializeApprovedApplications(ctx context.Context, userID uuid.UUID) error {
	u, err := s.q.GetUserByID(ctx, toPg(userID))
	if err != nil || u.Email == "" {
		return err
	}
	apps, err := s.q.ApprovedApplicationsForEmail(ctx, pgText(u.Email))
	if err != nil {
		return err
	}
	for _, a := range apps {
		orgID := fromPg(a.OrganizationID)
		err := s.withTx(ctx, func(q *db.Queries) error {
			n, err := q.LinkApplicationUser(ctx, db.LinkApplicationUserParams{
				ID: a.ID, UserID: toPg(userID),
			})
			if err != nil {
				return err
			}
			if n == 0 {
				return nil // a raced bind consumed it — skip
			}
			if _, err := q.GetMembership(ctx, db.GetMembershipParams{
				OrganizationID: a.OrganizationID, UserID: toPg(userID),
			}); err == nil {
				return nil // already a member (e.g. invited separately) — link only
			} else if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			org, err := q.GetOrg(ctx, a.OrganizationID)
			if err != nil {
				return err
			}
			return s.materializeMembership(ctx, q, fromPg(org.TenantID), orgID, fromPg(a.ID), userID, a.SponsorMemberID, userID)
		})
		if err != nil {
			// One bad application must not block the rest of the set or the
			// caller's login — surface it for ops instead.
			log.Printf("materialize application %s for user %s: %v", a.ID.Bytes, userID, err)
		}
	}
	return nil
}

// ResignSelf is the self-resign path for members who can't resolve an
// OrgCtx — a suspended member carries no org context but §11 still allows
// {mid}=self → resign. Builds the minimum OrgCtx and runs the normal
// machine so the terminal cleanup (seats, cards, events) is identical.
func (s *Service) ResignSelf(ctx context.Context, userID, orgID uuid.UUID) error {
	m, err := s.q.GetMembership(ctx, db.GetMembershipParams{
		OrganizationID: toPg(orgID), UserID: toPg(userID),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotMember
		}
		return err
	}
	org, err := s.q.GetOrg(ctx, toPg(orgID))
	if err != nil {
		return err
	}
	oc := &OrgCtx{OrgID: orgID, TenantID: fromPg(org.TenantID), UserID: userID, MembershipID: fromPg(m.ID)}
	return s.TransitionMember(ctx, oc, fromPg(m.ID), "resign")
}

// TransitionMember drives the ADR-014 lifecycle machine on the membership
// row. Intake verbs (screen/approve/reject/withdraw) run here too —
// invited members land at 'applied' with no application row, so the
// membership itself carries their intake decisions (approve mints
// member_no; sponsors aren't re-required — an invite is the org's vouch).
// Terminal transitions (expelled/resigned/deceased) vacate appointments +
// committee seats + cards in the same tx; every legal move emits its
// event and invalidates the member's perm/org-context caches (Rule 6).
func (s *Service) TransitionMember(ctx context.Context, oc *OrgCtx, memberID uuid.UUID, action string) error {
	to := memberActions[action]
	if to == "" {
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
	// ADR-014 guard: approved→registered requires the registration payment —
	// blocked while the member owes registration money (§12 gate).
	if to == "registered" && s.regGate != nil {
		pending, err := s.regGate(ctx, oc.OrgID, memberID)
		if err != nil {
			return err
		}
		if pending {
			return ErrRegistrationPending
		}
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.TransitionMembership(ctx, db.TransitionMembershipParams{
			ID: toPg(memberID), OrganizationID: toPg(oc.OrgID), LifecycleState: to, Column4: []string{from},
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition // raced with another transition
		} else if err != nil {
			return err
		}
		if to == "approved" && (!m.MemberNo.Valid || m.MemberNo.String == "") {
			// Membership-level approve mints the number the application
			// path would have minted at materialize-time.
			no, err := s.memberNo(ctx, q, oc.OrgID)
			if err != nil {
				return err
			}
			if err := q.SetMemberNo(ctx, db.SetMemberNoParams{ID: toPg(memberID), MemberNo: pgText(no)}); err != nil {
				return err
			}
		}
		if to == "active" {
			// §07 enforcement point: limit.members.active — the count
			// already includes this member post-transition; at exactly the
			// cap the join is allowed, beyond it blocked.
			if s.ent != nil {
				count, err := q.CountTenantActiveMembers(ctx, toPg(oc.TenantID))
				if err != nil {
					return err
				}
				if allowed, _ := s.ent.CheckLimit(ctx, oc.TenantID, "limit.members.active", count-1); !allowed {
					return ErrMemberLimitExceeded
				}
			}
			// activation is "joining" — the member role lands here for the
			// application path; invited members already hold it from accept
			// (org-vouched), so ON CONFLICT DO NOTHING keeps this idempotent.
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
			units, err := q.RemoveMemberCommitteeSeats(ctx, toPg(memberID))
			if err != nil {
				return err
			}
			// Rule 12: seat removal emits its event per unit — rbac consumers
			// dedupe on event id and invalidate scope caches.
			for _, u := range units {
				if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CommitteeMemberRemoved", "committee_membership", fromPg(u.ID), oc.UserID, true,
					map[string]any{"committee_membership_id": fromPg(u.ID), "org_unit_id": fromPg(u.OrgUnitID), "member_id": memberID, "org_id": oc.OrgID}); err != nil {
					return err
				}
			}
			if err := q.RevokeCards(ctx, toPg(memberID)); err != nil {
				return err
			}
		}
		// TODO: MemberDeceased → welfare bereavement-case candidate flags
		// when §13 welfare lands (sourced event already carries the hook).
		if name := transitionEvent(from, to); name != "" {
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, name, "organization_membership", memberID, oc.UserID, true,
				map[string]any{"membership_id": memberID, "org_id": oc.OrgID, "user_id": userID, "from": from, "to": to})
		}
		return nil
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

// Members lists the directory with optional category/state/unit filters.
func (s *Service) Members(ctx context.Context, oc *OrgCtx, category, state string, unitID uuid.UUID) ([]db.ListMembersRow, error) {
	return s.q.ListMembers(ctx, db.ListMembersParams{
		OrgID: toPg(oc.OrgID), Category: category, State: state, UnitID: pgUUIDPtr(unitID),
	})
}

func (s *Service) MemberDetail(ctx context.Context, oc *OrgCtx, memberID uuid.UUID) (db.GetMembershipFullRow, error) {
	m, err := s.q.GetMembershipFull(ctx, toPg(memberID))
	if errors.Is(err, pgx.ErrNoRows) || fromPg(m.OrganizationID) != oc.OrgID {
		return db.GetMembershipFullRow{}, ErrNotMember
	}
	return m, err
}

// PatchMember is the limited admin edit surface — category + home-unit
// scope; lifecycle never changes here (transitions are explicit).
func (s *Service) PatchMember(ctx context.Context, oc *OrgCtx, memberID uuid.UUID, category string, unitID uuid.UUID) error {
	if category != "" && !memberCategories[category] {
		return ErrBadCategory
	}
	if unitID != uuid.Nil {
		u, err := s.q.GetUnit(ctx, toPg(unitID))
		if err != nil || fromPg(u.OrganizationID) != oc.OrgID {
			return ErrBadScopeUnit // scope must be a unit of this org
		}
	}
	if _, err := s.q.PatchMembership(ctx, db.PatchMembershipParams{
		ID: toPg(memberID), OrgID: toPg(oc.OrgID), Category: pgText(category), OrgUnitID: pgUUIDPtr(unitID),
	}); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotMember
	} else {
		return err
	}
}

// WithdrawApplication is the applicant's self-service path (catalogued
// "self" node) — their own application only, and only while pre-decision.
func (s *Service) WithdrawApplication(ctx context.Context, userID, orgID, appID uuid.UUID) error {
	app, err := s.q.GetApplication(ctx, toPg(appID))
	if errors.Is(err, pgx.ErrNoRows) || fromPg(app.OrganizationID) != orgID || fromPg(app.UserID) != userID {
		return ErrApplicationNotFound
	}
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.WithdrawApplication(ctx, db.WithdrawApplicationParams{
			ID: toPg(appID), UserID: toPg(userID),
		}); errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition // already decided
		} else if err != nil {
			return err
		}
		return events.Emit(ctx, q, uuid.Nil, orgID, "MemberWithdrawn", "membership_application", appID, userID, true,
			map[string]any{"application_id": appID, "org_id": orgID})
	})
}

// RenewMember records a dues period (membership_renewals is UNIQUE per
// membership+period — re-attempts 409, never double-record). An optional
// payment_id links the settling payment (§12) — the FK validates it; the
// finance module is the authoritative record of that payment's state.
func (s *Service) RenewMember(ctx context.Context, oc *OrgCtx, memberID uuid.UUID, period string, startsAt, endsAt time.Time, paymentID uuid.UUID) (uuid.UUID, error) {
	m, err := s.MemberDetail(ctx, oc, memberID)
	if err != nil {
		return uuid.Nil, err
	}
	if m.LifecycleState != "active" && m.LifecycleState != "suspended" {
		return uuid.Nil, ErrInvalidTransition
	}
	if !endsAt.After(startsAt) {
		return uuid.Nil, ErrBadValue // renewal window must span forward
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
		if paymentID != uuid.Nil {
			n, err := q.SetRenewalPayment(ctx, db.SetRenewalPaymentParams{
				ID: toPg(id), OrganizationID: toPg(oc.OrgID), PaymentID: toPg(paymentID)})
			if err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23503" {
					return ErrBadValue // payment_id doesn't reference a real payment
				}
				return err
			}
			if n == 0 {
				return ErrBadValue // not a confirmed payment of THIS member in THIS org
			}
		}
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
	liveCard := func() (db.IssueCardRow, bool) {
		cur, err := s.q.GetActiveCard(ctx, toPg(memberID))
		if err != nil {
			return db.IssueCardRow{}, false
		}
		return db.IssueCardRow{ID: cur.ID, Serial: cur.Serial, IssuedAt: cur.IssuedAt}, true
	}
	// one active card per member (partial unique index) — re-issue returns
	// the live card instead of erroring or double-emitting.
	if c, ok := liveCard(); ok {
		return c, nil
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
	if err != nil && isUniqueViolation(err) {
		// Concurrent issue won the race on the partial unique index —
		// surface the winning card rather than a 500.
		if c, ok := liveCard(); ok {
			return c, nil
		}
	}
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

// StaleImportIDs lists imports stranded at 'processing' — the enqueue
// died, or the worker was archived mid-run. The scheduler:tick sweep
// re-drives them (mirrors the webhook stale-redrive pattern).
func (s *Service) StaleImportIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := s.q.ListStaleImports(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, fromPg(r))
	}
	return out, nil
}

// FailImport marks an import row dead — used when the durable record
// exists but the task could not be enqueued (better a visible failure
// than a row stuck at 'processing' until the sweep finds it).
func (s *Service) FailImport(ctx context.Context, importID uuid.UUID, reason string) error {
	return s.q.FinishImport(ctx, db.FinishImportParams{
		ID: toPg(importID), Status: "failed", Errors: mustJSON(map[string]any{"enqueue": reason}),
	})
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
		// Rule 27 idempotency: a retried worker skips rows that already
		// produced an open application for this applicant (email or phone).
		if exists, err := s.q.ExistsOpenApplicationByEmail(ctx, db.ExistsOpenApplicationByEmailParams{
			OrgID: imp.OrganizationID, Email: email, Phone: phone,
		}); err != nil {
			return err
		} else if exists {
			ok++ // already created — counts as success on replay
			continue
		}
		_, err := s.q.CreateApplication(ctx, db.CreateApplicationParams{
			OrganizationID: imp.OrganizationID, Email: pgText(email), Phone: pgText(phone),
			Answers: answers,
		})
		if err != nil {
			errs = append(errs, map[string]any{"row": i, "error": err.Error()})
			continue
		}
		ok++
	}
	if err := s.q.FinishImport(ctx, db.FinishImportParams{
		ID: toPg(importID), Status: "done",
		RowCount: pgtype.Int4{Int32: int32(len(rows)), Valid: true}, OkCount: pgtype.Int4{Int32: int32(ok), Valid: true}, ErrCount: pgtype.Int4{Int32: int32(len(errs)), Valid: true},
		Errors: mustJSON(errs),
	}); err != nil {
		return err
	}
	// Catalogued payload {import_id,ok,err}, sourced=no — per-application
	// MemberApplied isn't emitted for imports (intake, not membership yet).
	return s.withTx(ctx, func(q *db.Queries) error {
		return events.Emit(ctx, q, uuid.Nil, orgID, "MemberImported", "member_import", importID, uuid.Nil, false,
			map[string]any{"import_id": importID, "ok": ok, "err": len(errs)})
	})
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}

// ---------- export (membership:export — durable row + worker) ----------

// RequestExport records the durable export row the worker fills — the
// handler enqueues membership:export and returns the id (Rule 8).
func (s *Service) RequestExport(ctx context.Context, oc *OrgCtx, filename string) (uuid.UUID, error) {
	if filename == "" {
		filename = "members.csv"
	}
	id, err := s.q.CreateExport(ctx, db.CreateExportParams{
		OrganizationID: toPg(oc.OrgID), RequestedBy: toPg(oc.MembershipID), Filename: filename})
	return fromPg(id), err
}

// ExportDownload fetches a finished export (org-scoped).
func (s *Service) ExportDownload(ctx context.Context, oc *OrgCtx, eid uuid.UUID) (*db.MemberExports, error) {
	e, err := s.q.GetExport(ctx, db.GetExportParams{ID: toPg(eid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, ErrImportNotFound
	}
	return &e, nil
}

// ProcessExport generates the CSV into the durable row — idempotent: a
// retried worker rewrites the same row.
func (s *Service) ProcessExport(ctx context.Context, exportID uuid.UUID) error {
	e, err := s.q.GetExportAny(ctx, toPg(exportID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrImportNotFound
	}
	if err != nil {
		return err
	}
	rows, err := s.q.ListMembersForExport(ctx, e.OrganizationID)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("member_no,display_name,email,category,lifecycle_state,dues_standing,joined_at\n")
	for _, m := range rows {
		joined := ""
		if m.JoinedAt.Valid {
			joined = m.JoinedAt.Time.Format("2006-01-02")
		}
		fmt.Fprintf(&b, "%s,%s,%s,%s,%s,%s,%s\n",
			m.MemberNo.String, csvCell(m.DisplayName), csvCell(m.Email), m.Category, m.LifecycleState, m.DuesStanding, joined)
	}
	return s.q.FinishExport(ctx, db.FinishExportParams{
		ID: e.ID, Status: "done", Content: []byte(b.String()),
		RowCount: pgtype.Int4{Int32: int32(len(rows)), Valid: true}})
}

func csvCell(s string) string {
	if strings.ContainsAny(s, ",\"\n") {
		return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
	}
	return s
}
