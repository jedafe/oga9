// Ref: oga9-engineering/sections/03-rbac-positions.md (Phase 04)
// Permission resolution: union(tenant role grants) + union(active
// current-term position grants) + _branch/_committee scope suffixes.
// Cached at oga9:perm:{user}:{org} (5 min, Rule 6 invalidation).
package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jedafe/oga9/internal/db"
)

type permSet struct {
	Nodes       map[string]bool `json:"n"`
	Branch      map[string]bool `json:"b"` // base nodes granted via branch-scoped appointments
	Committee   map[string]bool `json:"c"` // base nodes granted via committee scope/membership
	InCommittee bool            `json:"ic"`
}

// ResolvePermissions computes the user's effective node set for an org.
func (s *Service) ResolvePermissions(ctx context.Context, userID, orgID uuid.UUID) (*permSet, error) {
	key := "oga9:perm:" + userID.String() + ":" + orgID.String()
	if raw, err := s.vk.Do(ctx, s.vk.B().Get().Key(key).Build()).AsBytes(); err == nil {
		var ps permSet
		if json.Unmarshal(raw, &ps) == nil && ps.Nodes != nil {
			return &ps, nil
		}
	}
	ps := &permSet{Nodes: map[string]bool{}, Branch: map[string]bool{}, Committee: map[string]bool{}}
	nodes, err := s.q.ResolveOrgPermissions(ctx, db.ResolveOrgPermissionsParams{OrgID: toPg(orgID), UserID: toPg(userID)})
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		ps.Nodes[n] = true
	}
	scoped, err := s.q.ScopedPositionNodes(ctx, db.ScopedPositionNodesParams{OrgID: toPg(orgID), UserID: toPg(userID)})
	if err != nil {
		return nil, err
	}
	for _, sc := range scoped {
		if sc.BranchID.Valid {
			ps.Branch[sc.Permission] = true
		}
		if sc.CommitteeID.Valid {
			ps.Committee[sc.Permission] = true
		}
	}
	units, err := s.q.UserCommitteeUnits(ctx, db.UserCommitteeUnitsParams{OrgID: toPg(orgID), UserID: toPg(userID)})
	if err != nil {
		return nil, err
	}
	ps.InCommittee = len(units) > 0
	if b, err := json.Marshal(ps); err == nil {
		_ = s.vk.Do(ctx, s.vk.B().Set().Key(key).Value(string(b)).ExSeconds(300).Build()).Error()
	}
	return ps, nil
}

// HasPermission — Rule 20: nodes, never position names. _branch/_committee
// resolve against the scoped grant set; all other suffixes are literal
// seeded nodes.
func (s *Service) HasPermission(ctx context.Context, userID, orgID uuid.UUID, node string) bool {
	ps, err := s.ResolvePermissions(ctx, userID, orgID)
	if err != nil {
		return false
	}
	if ps.Nodes[node] {
		return true
	}
	if base, ok := strings.CutSuffix(node, "_branch"); ok {
		return ps.Branch[base] || ps.Branch[node]
	}
	// QUESTION: §03 lists committee_memberships as a Rule-6 trigger but
	// doesn't say whether membership alone scopes base nodes — read as
	// conferring _committee on nodes the member already holds. Flag for
	// review; tighten to appointment-only if that's wrong.
	if base, ok := strings.CutSuffix(node, "_committee"); ok {
		return ps.Committee[base] || ps.Committee[node] || (ps.InCommittee && ps.Nodes[base])
	}
	return false
}

// InvalidatePermCache — Rule 6 trigger point: drop oga9:perm:{user}:{org}.
func (s *Service) InvalidatePermCache(ctx context.Context, userID, orgID uuid.UUID) {
	_ = s.vk.Do(ctx, s.vk.B().Del().Key("oga9:perm:"+userID.String()+":"+orgID.String()).Build()).Error()
}

// ---------- positions / terms / appointments (service ops) ----------

// CreatePosition adds an org position; permission nodes attach via
// SetPositionPermissions (nodes must be seeded — FK enforces catalog).
func (s *Service) CreatePosition(ctx context.Context, oc *OrgCtx, name, slug string, executive bool, seats int32, nodes []string) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		pid, err := q.CreatePosition(ctx, db.CreatePositionParams{
			OrganizationID: toPg(oc.OrgID), Name: name, Slug: slug,
			IsExecutive: executive, Seats: seats,
		})
		if err != nil {
			return err
		}
		id = fromPg(pid)
		for _, n := range nodes {
			if err := q.AssignPositionPermission(ctx, db.AssignPositionPermissionParams{
				PositionID: pid, Permission: n,
			}); err != nil {
				return err
			}
		}
		return emit(ctx, q, oc.TenantID, oc.OrgID, "PositionCreated", "org_position", id, oc.UserID, true,
			map[string]any{"position_id": id, "org_id": oc.OrgID, "name": name})
	})
	return id, err
}

// CreateTerm starts an EXCO cohort period (Rule 21). Only one current term
// per org — the partial unique index enforces it.
func (s *Service) CreateTerm(ctx context.Context, oc *OrgCtx, label string, startsAt, endsAt time.Time, current bool) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		tid, err := q.CreateTerm(ctx, db.CreateTermParams{
			OrganizationID: toPg(oc.OrgID), Label: label,
			StartsAt: pgDate(startsAt), EndsAt: pgDate(endsAt), IsCurrent: current,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrTermConflict // a current term already exists
			}
			return err
		}
		id = fromPg(tid)
		return emit(ctx, q, oc.TenantID, oc.OrgID, "TermStarted", "term", id, oc.UserID, true,
			map[string]any{"term_id": id, "org_id": oc.OrgID})
	})
	return id, err
}

// AppointOfficer — term-scoped appointment (Rule 21). Emits LeaderAppointed
// (catalogued) and invalidates the appointee's perm cache (Rule 6).
func (s *Service) AppointOfficer(ctx context.Context, oc *OrgCtx, termID, positionID, memberID, branchID, committeeID uuid.UUID, startsAt time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	var appointee uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		m, err := q.GetMembershipByID(ctx, toPg(memberID))
		if err != nil || fromPg(m.OrganizationID) != oc.OrgID {
			return ErrNotMember
		}
		// position, term and scope units must all belong to this org —
		// a foreign id would graft another org's grants into this one.
		po, err := q.GetPositionOrg(ctx, toPg(positionID))
		if err != nil || fromPg(po) != oc.OrgID {
			return ErrNotFound
		}
		to, err := q.GetTermOrg(ctx, toPg(termID))
		if err != nil || fromPg(to.OrganizationID) != oc.OrgID {
			return ErrNotFound
		}
		for _, u := range []uuid.UUID{branchID, committeeID} {
			if u == uuid.Nil {
				continue
			}
			ur, err := q.GetUnit(ctx, toPg(u))
			if err != nil || fromPg(ur.OrganizationID) != oc.OrgID {
				return ErrNotFound
			}
		}
		appointee = fromPg(m.UserID)
		aid, err := q.AppointOfficer(ctx, db.AppointOfficerParams{
			OrganizationID: toPg(oc.OrgID), TermID: toPg(termID), PositionID: toPg(positionID),
			MemberID: toPg(memberID), BranchID: pgUUIDPtr(branchID), CommitteeID: pgUUIDPtr(committeeID),
			StartsAt: pgDate(startsAt),
		})
		if err != nil {
			return err
		}
		id = fromPg(aid)
		return emit(ctx, q, oc.TenantID, oc.OrgID, "LeaderAppointed", "officer_appointment", id, oc.UserID, true,
			map[string]any{"appointment_id": id, "position_id": positionID, "term_id": termID, "org_id": oc.OrgID})
	})
	if err == nil {
		s.InvalidatePermCache(ctx, appointee, oc.OrgID) // Rule 6
	}
	return id, err
}

// EndAppointment ends an appointment early → AppointmentEnded + invalidate.
func (s *Service) EndAppointment(ctx context.Context, oc *OrgCtx, appointmentID uuid.UUID) error {
	var appointee uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		row, err := q.EndAppointment(ctx, db.EndAppointmentParams{ID: toPg(appointmentID), OrgID: toPg(oc.OrgID)})
		if err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound // unknown, ended, or foreign appointment
			}
			return err
		}
		m, err := q.GetMembershipByID(ctx, row.MemberID)
		if err != nil {
			return err
		}
		appointee = fromPg(m.UserID)
		return emit(ctx, q, oc.TenantID, oc.OrgID, "AppointmentEnded", "officer_appointment", appointmentID, oc.UserID, true,
			map[string]any{"appointment_id": appointmentID, "org_id": oc.OrgID})
	})
	if err == nil && appointee != uuid.Nil {
		s.InvalidatePermCache(ctx, appointee, oc.OrgID)
	}
	return err
}

// CloseTerm — term manager (04.05): close the current term, end all open
// appointments, emit AppointmentEnded per appointment, invalidate each
// appointee's perm cache. Institutional history survives: rows stay.
func (s *Service) CloseTerm(ctx context.Context, oc *OrgCtx, termID uuid.UUID) error {
	var appointees []uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		if err := q.CloseTerm(ctx, db.CloseTermParams{ID: toPg(termID), OrgID: toPg(oc.OrgID)}); err != nil {
			return err
		}
		ended, err := q.EndTermAppointments(ctx, toPg(termID))
		if err != nil {
			return err
		}
		for _, ap := range ended {
			m, err := q.GetMembershipByID(ctx, ap.MemberID)
			if err != nil {
				return err
			}
			appointees = append(appointees, fromPg(m.UserID))
			if err := emit(ctx, q, oc.TenantID, oc.OrgID, "AppointmentEnded", "officer_appointment", fromPg(ap.ID), oc.UserID, true,
				map[string]any{"appointment_id": fromPg(ap.ID), "term_id": termID, "org_id": oc.OrgID}); err != nil {
				return err
			}
		}
		return nil
	})
	for _, u := range appointees {
		s.InvalidatePermCache(ctx, u, oc.OrgID)
	}
	return err
}

// AddCommitteeMember / RemoveCommitteeMember — Rule 6 triggers.
func (s *Service) AddCommitteeMember(ctx context.Context, oc *OrgCtx, unitID, memberID uuid.UUID, role string) error {
	var userID uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		m, err := q.GetMembershipByID(ctx, toPg(memberID))
		if err != nil || fromPg(m.OrganizationID) != oc.OrgID {
			return ErrNotMember
		}
		u, err := q.GetUnit(ctx, toPg(unitID)) // committee unit must be ours
		if err != nil || fromPg(u.OrganizationID) != oc.OrgID || u.Type != "committee" {
			return ErrNotFound
		}
		userID = fromPg(m.UserID)
		_, err = q.AddCommitteeMember(ctx, db.AddCommitteeMemberParams{
			OrganizationID: toPg(oc.OrgID), OrgUnitID: toPg(unitID), MemberID: toPg(memberID), Role: role,
		})
		return err
	})
	if err == nil {
		s.InvalidatePermCache(ctx, userID, oc.OrgID)
	}
	return err
}

func (s *Service) RemoveCommitteeMember(ctx context.Context, oc *OrgCtx, cmID uuid.UUID) error {
	var userID uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		mid, err := q.RemoveCommitteeMember(ctx, db.RemoveCommitteeMemberParams{ID: toPg(cmID), OrgID: toPg(oc.OrgID)})
		if err != nil {
			if err == pgx.ErrNoRows {
				return ErrNotFound
			}
			return err
		}
		m, err := q.GetMembershipByID(ctx, mid)
		if err != nil {
			return err
		}
		userID = fromPg(m.UserID)
		return nil
	})
	if err == nil {
		s.InvalidatePermCache(ctx, userID, oc.OrgID)
	}
	return err
}
