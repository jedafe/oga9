// Ref: oga9-engineering/sections/27-volunteers.md — named volunteer roles
// on an activity + member signups (signed_up→confirmed→completed|no_show,
// standby promotion, activity cancel cascades via ActivityCancelled).
package activities

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrRoleFull      = errors.New("activities: role_full")
	ErrNotEligible   = errors.New("activities: not_eligible")
	ErrAlreadySigned = errors.New("activities: already_signed_up")
)

// ---------- roles ----------

type VolunteerRoleIn struct {
	Name   string    `json:"name"`
	Slots  int       `json:"slots"`
	Note   string    `json:"note"`
	UnitID uuid.UUID `json:"unit_id"`
}

func (s *Service) CreateVolunteerRole(ctx context.Context, oc *tenant.OrgCtx, activityID uuid.UUID, in VolunteerRoleIn) (uuid.UUID, error) {
	if in.Name == "" {
		return uuid.Nil, ErrBadValue
	}
	if in.Slots <= 0 {
		in.Slots = 1
	}
	if _, err := s.q.GetActivity(ctx, db.GetActivityParams{ID: toPg(activityID), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return uuid.Nil, ErrNotFound
	}
	row, err := s.q.CreateVolunteerRole(ctx, db.CreateVolunteerRoleParams{
		OrganizationID: toPg(oc.OrgID), ActivityID: toPg(activityID),
		UnitID: toPg(in.UnitID), Name: in.Name, Slots: int32(in.Slots),
		Note: pgText(in.Note), CreatedBy: toPg(oc.MembershipID)})
	return fromPg(row.ID), err
}

func (s *Service) VolunteerRoles(ctx context.Context, oc *tenant.OrgCtx, activityID uuid.UUID) ([]db.VolunteerRoles, error) {
	return s.q.ListVolunteerRoles(ctx, db.ListVolunteerRolesParams{
		ActivityID: toPg(activityID), OrganizationID: toPg(oc.OrgID)})
}

func (s *Service) PatchVolunteerRole(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID, in VolunteerRoleIn, clearUnit bool) error {
	n, err := s.q.UpdateVolunteerRole(ctx, db.UpdateVolunteerRoleParams{
		ID: toPg(rid), OrganizationID: toPg(oc.OrgID),
		Name: pgTextOr(in.Name), Slots: intOr(in.Slots), Note: pgTextOr(in.Note),
		UnitID: toPg(in.UnitID), ClearUnit: clearUnit})
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}

func (s *Service) DeleteVolunteerRole(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID) error {
	n, err := s.q.DeleteVolunteerRole(ctx, db.DeleteVolunteerRoleParams{ID: toPg(rid), OrganizationID: toPg(oc.OrgID)})
	if err == nil && n == 0 {
		err = ErrInvalidTransition // has confirmed signups or foreign
	}
	return err
}

// ---------- signups ----------

// Signup — eligibility: unit-restricted roles require membership in the
// unit tree; when the org rule doesn't require confirmation the signup
// confirms immediately while slots remain (VolunteerConfirmed still emits).
func (s *Service) Signup(ctx context.Context, oc *tenant.OrgCtx, activityID, roleID uuid.UUID, note string) (uuid.UUID, error) {
	return s.signupTx(ctx, oc, activityID, roleID, oc.MembershipID, note)
}

func (s *Service) signupTx(ctx context.Context, oc *tenant.OrgCtx, activityIDGuard, roleID, memberID uuid.UUID, note string) (uuid.UUID, error) {
	var sid uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		role, err := q.LockVolunteerRole(ctx, db.LockVolunteerRoleParams{ID: toPg(roleID), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		full, err := q.GetVolunteerRole(ctx, db.GetVolunteerRoleParams{ID: toPg(roleID), OrganizationID: toPg(oc.OrgID)})
		if err != nil || fromPg(full.ActivityID) != activityIDGuard {
			return ErrNotFound // role lives under a different activity
		}
		if full.UnitID.Valid {
			ok, err := q.MemberInUnitTree(ctx, db.MemberInUnitTreeParams{
				ID: full.UnitID, OrganizationID: toPg(oc.OrgID), ID_2: toPg(memberID)})
			if err != nil {
				return err
			}
			if !ok {
				return ErrNotEligible
			}
		}
		row, err := q.SignupVolunteer(ctx, db.SignupVolunteerParams{
			RoleID: role.ID, MembershipID: toPg(memberID), Note: pgText(note)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrAlreadySigned
		}
		if err != nil {
			return err
		}
		sid = fromPg(row.ID)
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VolunteerSignedUp",
			"volunteer_signup", sid, oc.UserID, false,
			map[string]any{"signup_id": sid, "role_id": roleID, "membership_id": memberID}); err != nil {
			return err
		}
		// require_confirmation=false → auto-confirm under capacity.
		if !s.orgSettingBool(ctx, oc.OrgID, "org.volunteers.require_confirmation", true) {
			confirmed, err := q.ConfirmedCount(ctx, role.ID)
			if err != nil {
				return err
			}
			if int64(confirmed) < int64(role.Slots) {
				if err := s.confirmSignupTx(ctx, q, oc, toPg(sid), role.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return sid, err
}

// Confirm — officer confirms a signup; the role row is locked so capacity
// checks are race-free. Full roles leave the signup queued as standby.
func (s *Service) ConfirmSignup(ctx context.Context, oc *tenant.OrgCtx, signupID uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		su, err := q.GetSignup(ctx, db.GetSignupParams{ID: toPg(signupID), OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		role, err := q.LockVolunteerRole(ctx, db.LockVolunteerRoleParams{
			ID: su.RoleID, OrganizationID: toPg(oc.OrgID)})
		if err != nil {
			return ErrNotFound
		}
		confirmed, err := q.ConfirmedCount(ctx, su.RoleID)
		if err != nil {
			return err
		}
		if int64(confirmed) >= int64(role.Slots) {
			return ErrRoleFull // stays signed_up — standby queue (§27)
		}
		return s.confirmSignupTx(ctx, q, oc, toPg(signupID), su.RoleID)
	})
}

func (s *Service) confirmSignupTx(ctx context.Context, q *db.Queries, oc *tenant.OrgCtx, signupID, roleID pgtype.UUID) error {
	row, err := q.ConfirmSignup(ctx, db.ConfirmSignupParams{
		ID: signupID, OrganizationID: toPg(oc.OrgID), DecidedBy: toPg(oc.MembershipID)})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrInvalidTransition
	}
	if err != nil {
		return err
	}
	return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VolunteerConfirmed",
		"volunteer_signup", fromPg(row.ID), oc.UserID, true,
		map[string]any{"signup_id": fromPg(row.ID), "role_id": fromPg(row.RoleID),
			"membership_id": fromPg(row.MembershipID)})
}

// Withdraw — self-service cancel (member's own signup only).
func (s *Service) Withdraw(ctx context.Context, oc *tenant.OrgCtx, signupID uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.CancelSignupSelf(ctx, db.CancelSignupSelfParams{
			ID: toPg(signupID), MembershipID: toPg(oc.MembershipID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := s.cancelledTx(ctx, q, oc, row, "self"); err != nil {
			return err
		}
		return s.promoteStandby(ctx, q, oc, row.RoleID)
	})
}

// Cancel — officer cancels any open signup; a freed confirmed slot
// promotes the oldest standby signup (§27).
func (s *Service) CancelSignup(ctx context.Context, oc *tenant.OrgCtx, signupID uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.CancelSignup(ctx, db.CancelSignupParams{
			ID: toPg(signupID), OrganizationID: toPg(oc.OrgID), DecidedBy: toPg(oc.MembershipID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if err := s.cancelledTx(ctx, q, oc, row, "officer"); err != nil {
			return err
		}
		return s.promoteStandby(ctx, q, oc, row.RoleID)
	})
}

func (s *Service) cancelledTx(ctx context.Context, q *db.Queries, oc *tenant.OrgCtx, row db.VolunteerSignups, by string) error {
	return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VolunteerCancelled",
		"volunteer_signup", fromPg(row.ID), oc.UserID, true,
		map[string]any{"signup_id": fromPg(row.ID), "role_id": fromPg(row.RoleID), "by": by})
}

func (s *Service) promoteStandby(ctx context.Context, q *db.Queries, oc *tenant.OrgCtx, roleID pgtype.UUID) error {
	role, err := q.LockVolunteerRole(ctx, db.LockVolunteerRoleParams{ID: roleID, OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil // role gone — nothing to promote into
	}
	confirmed, err := q.ConfirmedCount(ctx, roleID)
	if err != nil {
		return err
	}
	if int64(confirmed) >= int64(role.Slots) {
		return nil
	}
	next, err := q.OldestStandby(ctx, roleID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.confirmSignupTx(ctx, q, oc, next.ID, roleID)
}

// Mark — post-activity officer marks: completed|no_show (auditable marks,
// suggestion from §13 check-in stays manual per §27).
func (s *Service) MarkSignup(ctx context.Context, oc *tenant.OrgCtx, signupID uuid.UUID, state string) error {
	if state != "completed" && state != "no_show" {
		return ErrBadValue
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.MarkSignup(ctx, db.MarkSignupParams{
			ID: toPg(signupID), OrganizationID: toPg(oc.OrgID),
			State: state, DecidedBy: toPg(oc.MembershipID)})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrInvalidTransition
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VolunteerMarked",
			"volunteer_signup", fromPg(row.ID), oc.UserID, false,
			map[string]any{"signup_id": fromPg(row.ID), "role_id": fromPg(row.RoleID), "state": state, "by": oc.MembershipID})
	})
}

func (s *Service) Roster(ctx context.Context, oc *tenant.OrgCtx, activityID uuid.UUID) ([]db.ActivityRosterRow, error) {
	return s.q.ActivityRoster(ctx, db.ActivityRosterParams{
		ActivityID: toPg(activityID), OrganizationID: toPg(oc.OrgID)})
}

func (s *Service) MyVolunteerSignups(ctx context.Context, userID uuid.UUID) ([]db.MyVolunteerSignupsRow, error) {
	return s.q.MyVolunteerSignups(ctx, toPg(userID))
}

// ---------- sweeps + consumers ----------

// VolunteerRemind — activities:volunteer_remind: confirmed volunteers get
// a notification org.volunteers.remind_hours before start. Dedupe rides
// the notification row (ref = signup) — at-least-once safe (Rule 27).
func (s *Service) VolunteerRemind(ctx context.Context, notify func(ctx context.Context, q *db.Queries, e events.Event, userID uuid.UUID, kind, title, body, refType string, refID pgtype.UUID) error) (int, error) {
	rows, err := s.q.ConfirmedVolunteersDue(ctx, 24)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range rows {
		err := s.withTx(ctx, func(q *db.Queries) error {
			tid, _ := q.GetOrgTenantID(ctx, r.OrganizationID)
			return notify(ctx, q, events.Event{
				TenantID: fromPg(tid), OrganizationID: fromPg(r.OrganizationID)},
				fromPg(r.UserID), "volunteer_reminder",
				"Volunteer reminder: "+r.ActivityTitle, "Role: "+r.RoleName,
				"volunteer_signup", r.SignupID)
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ActivityCancelledConsumer — §27: cancel an activity and its signups
// cancel via this consumer (VolunteerCancelled emitted per signup).
func (s *Service) ActivityCancelledConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "ActivityCancelled" {
		return nil
	}
	aid := e.UUID("activity_id")
	if aid == uuid.Nil {
		aid = e.AggregateID
	}
	if aid == uuid.Nil {
		return nil
	}
	rows, err := q.CancelSignupsForActivity(ctx, toPg(aid))
	if err != nil {
		return err
	}
	for _, r := range rows {
		if err := events.Emit(ctx, q, e.TenantID, e.OrganizationID, "VolunteerCancelled",
			"volunteer_signup", fromPg(r.ID), e.ActorID, true,
			map[string]any{"signup_id": fromPg(r.ID), "role_id": fromPg(r.RoleID),
				"by": "activity_cancelled"}); err != nil {
			return err
		}
	}
	return nil
}

func pgTextOr(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
func intOr(n int) pgtype.Int4 {
	if n <= 0 {
		return pgtype.Int4{}
	}
	return pgtype.Int4{Int32: int32(n), Valid: true}
}
