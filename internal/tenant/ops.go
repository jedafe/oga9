// Ref: oga9-engineering/sections/10-organization-core.md
// Units, settings, invitations, branding, public directory.
package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/events"
)

var ErrBadValue = errors.New("invalid json value")
var ErrBadSlug = errors.New("invalid_slug")
var ErrBadCategory = errors.New("invalid_category")
var ErrBadRole = errors.New("invalid_role")
var ErrBadOrgType = errors.New("invalid_org_type")

var unitTypes = map[string]bool{"hq": true, "branch": true, "chapter": true, "committee": true, "wing": true}

// Member categories per organization_memberships schema (§04).
var memberCategories = map[string]bool{
	"floor": true, "executive": true, "life": true,
	"honorary": true, "patron": true, "trustee": true,
}

// Committee seat roles per committee_memberships schema (§03).
var committeeRoles = map[string]bool{"chairperson": true, "secretary": true, "member": true}

// Slugs are URL-path segments (directory, admin.{org}.host routing) —
// lowercase alnum + interior hyphens only.
func validSlug(s string) bool {
	if s == "" || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-':
			if i == 0 || i == len(s)-1 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// humanizeSlug renders a seed slug for display: "welfare_officer" →
// "Welfare Officer". Only used for template-seeded names.
func humanizeSlug(s string) string {
	parts := strings.FieldsFunc(s, func(r rune) bool { return r == '_' || r == '-' })
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// orgSettingsCatalog — every key registered in
// conventions/system-settings-catalog.md (Rule 23 — unknown keys rejected).
// Patterned keys (org.welfare.cap.{kind}_minor, once_per.{kind}_days) match
// via orgSettingKeyAllowed.
var orgSettingsCatalog = map[string]bool{
	"org.meeting.quorum_percent": true, "org.meeting.notice_days": true,
	"org.term.length_months": true, "org.dues.default_amount_minor": true,
	"org.dues.currency": true, "org.listing.is_public_listed": true,
	"org.membership.sponsor_required": true, "org.membership.number_format": true,
	"org.membership.app_fields": true, "org.finance.grace_days": true,
	"org.finance.defaulter_fine_minor": true, "org.finance.statement_visibility": true,
	"org.finance.drives.show_contributors": true, "org.finance.claims.require_receipt": true,
	"org.attendance.late_fine_minor": true, "org.attendance.absent_fine_minor": true,
	"org.welfare.min_membership_days": true, "org.welfare.requires_good_standing": true,
	"org.projects.update_interval_days": true,
	"org.chat.general_post_scope":       true, "org.chat.voice_max_seconds": true,
	"org.chat.edit_window_minutes": true, "org.chat.retention_days": true,
}

// orgSettingKeyAllowed allows catalogued pattern keys of the form
// org.welfare.cap.{kind}_minor / org.welfare.once_per.{kind}_days.
func orgSettingKeyAllowed(key string) bool {
	if orgSettingsCatalog[key] {
		return true
	}
	if strings.HasPrefix(key, "org.welfare.cap.") && strings.HasSuffix(key, "_minor") {
		return true
	}
	if strings.HasPrefix(key, "org.welfare.once_per.") && strings.HasSuffix(key, "_days") {
		return true
	}
	return false
}

type Unit struct {
	ID         uuid.UUID  `json:"id"`
	ParentID   *uuid.UUID `json:"parent_unit_id"`
	Type       string     `json:"type"`
	LevelLabel string     `json:"level_label"`
	Name       string     `json:"name"`
	Slug       string     `json:"slug"`
}

func toUnit(r db.ListUnitsRow) Unit {
	var p *uuid.UUID
	if r.ParentUnitID.Valid {
		u := fromPg(r.ParentUnitID)
		p = &u
	}
	return Unit{ID: fromPg(r.ID), ParentID: p, Type: r.Type,
		LevelLabel: r.LevelLabel.String, Name: r.Name, Slug: r.Slug}
}

func (s *Service) ListUnits(ctx context.Context, orgID uuid.UUID) ([]Unit, error) {
	rows, err := s.q.ListUnits(ctx, toPg(orgID))
	if err != nil {
		return nil, err
	}
	out := make([]Unit, len(rows))
	for i, r := range rows {
		out[i] = toUnit(r)
	}
	return out, nil
}

// CreateUnit validates same-org parent + DAG (no self-parent via DB; cycle
// impossible on create since the node doesn't exist yet). §07 enforcement
// point: limit.branches caps member-capable units (hq|branch|chapter) —
// enforced on branch|chapter creation (the org's hq is always allowed).
func (s *Service) CreateUnit(ctx context.Context, oc *OrgCtx, parentID uuid.UUID, typ, label, name, slug string, ent *entitlements.Service) (uuid.UUID, error) {
	if !unitTypes[typ] {
		return uuid.Nil, ErrBadUnitType
	}
	if !validSlug(slug) {
		return uuid.Nil, ErrBadSlug
	}
	if parentID != uuid.Nil {
		p, err := s.q.GetUnit(ctx, toPg(parentID))
		if err != nil {
			return uuid.Nil, ErrUnitNotFound
		}
		if fromPg(p.OrganizationID) != oc.OrgID {
			return uuid.Nil, ErrUnitNotFound // parent must be same-org (DAG)
		}
	}
	var id pgtype.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		if ent != nil && (typ == "branch" || typ == "chapter") {
			n, err := q.CountMemberCapableUnits(ctx, toPg(oc.OrgID))
			if err != nil {
				return err
			}
			if allowed, _ := ent.CheckLimit(ctx, oc.TenantID, "limit.branches", n); !allowed {
				return ErrLimitExceeded
			}
		}
		r, err := q.CreateUnit(ctx, db.CreateUnitParams{
			OrganizationID: toPg(oc.OrgID), ParentUnitID: pgUUIDPtr(parentID),
			Type: typ, LevelLabel: pgText(label), Name: name, Slug: slug,
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrSlugTaken // sibling slug collision
			}
			return err
		}
		id = r
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "OrgUnitCreated", "org_unit", fromPg(r), oc.UserID, true,
			map[string]any{"unit_id": fromPg(r), "org_id": oc.OrgID, "organization_id": oc.OrgID, "type": typ}); err != nil {
			return err
		}
		// Committee units also fire the governance event — analytics-style
		// consumers shouldn't have to know committees ride the unit tree.
		if typ == "committee" {
			return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "CommitteeCreated", "org_unit", fromPg(r), oc.UserID, true,
				map[string]any{"unit_id": fromPg(r), "org_id": oc.OrgID})
		}
		return nil
	})
	return fromPg(id), err
}

// UpdateUnit reparents a unit — walks the new parent chain to the root to
// prove the move can't create a cycle (§10 DAG).
func (s *Service) UpdateUnit(ctx context.Context, oc *OrgCtx, unitID, parentID uuid.UUID, typ, label, name string) error {
	u, err := s.q.GetUnit(ctx, toPg(unitID))
	if err != nil {
		if err == pgx.ErrNoRows {
			return ErrUnitNotFound
		}
		return err
	}
	if fromPg(u.OrganizationID) != oc.OrgID {
		return ErrUnitNotFound // cross-org access → 404, not 403 (no existence leak)
	}
	if !unitTypes[typ] {
		return ErrBadUnitType
	}
	// Retyping a committee with seats/scoped appointments strands them —
	// the seat tables would keep resolving scope for a unit that no longer
	// is a committee. Same dependents guard as ArchiveUnit.
	if u.Type != typ {
		if has, err := s.q.UnitHasActiveMembers(ctx, toPg(unitID)); err != nil {
			return err
		} else if has {
			return ErrUnitDependents
		}
	}
	if parentID == unitID {
		return ErrUnitCycle
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		// Walk the new parent chain inside the tx — the check and the write
		// must be atomic or two concurrent reparents could form a cycle.
		if parentID != uuid.Nil {
			seen := map[uuid.UUID]bool{unitID: true}
			cur := parentID
			for cur != uuid.Nil {
				if seen[cur] {
					return ErrUnitCycle
				}
				seen[cur] = true
				p, err := q.GetUnit(ctx, toPg(cur))
				if err != nil {
					if err == pgx.ErrNoRows {
						return ErrUnitNotFound
					}
					return err
				}
				if fromPg(p.OrganizationID) != oc.OrgID {
					return ErrUnitNotFound
				}
				cur = fromPg(p.ParentUnitID)
			}
		}
		if _, err := q.UpdateUnit(ctx, db.UpdateUnitParams{
			ID: toPg(unitID), Name: name, ParentUnitID: pgUUIDPtr(parentID),
			Type: typ, LevelLabel: pgText(label),
		}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "OrgUnitUpdated", "org_unit", unitID, oc.UserID, true,
			map[string]any{"unit_id": unitID, "org_id": oc.OrgID, "organization_id": oc.OrgID})
	})
}

// ArchiveUnit soft-archives (§10: blocked while child units OR member
// references exist — committee memberships and open appointments scoped
// to the unit count as dependents).
func (s *Service) ArchiveUnit(ctx context.Context, oc *OrgCtx, unitID uuid.UUID) error {
	u, err := s.q.GetUnit(ctx, toPg(unitID))
	if err != nil {
		if err == pgx.ErrNoRows {
			return ErrUnitNotFound
		}
		return err
	}
	if fromPg(u.OrganizationID) != oc.OrgID {
		return ErrUnitNotFound
	}
	if has, err := s.q.UnitHasActiveChildren(ctx, toPg(unitID)); err != nil {
		return err
	} else if has {
		return ErrUnitDependents
	}
	if has, err := s.q.UnitHasActiveMembers(ctx, toPg(unitID)); err != nil {
		return err
	} else if has {
		return ErrUnitDependents
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.ArchiveUnit(ctx, toPg(unitID)); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "OrgUnitArchived", "org_unit", unitID, oc.UserID, true,
			map[string]any{"unit_id": unitID, "org_id": oc.OrgID, "organization_id": oc.OrgID})
	})
}

// UpdateOrgProfile — §04 PATCH: name/geo/directory opt-in via org.admin.settings.
func (s *Service) UpdateOrgProfile(ctx context.Context, oc *OrgCtx, name, country, city string, listed bool) error {
	if name == "" {
		return ErrBadValue
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.UpdateOrgProfile(ctx, db.UpdateOrgProfileParams{
			ID: toPg(oc.OrgID), Name: name, CountryCode: pgText(country),
			City: pgText(city), IsPublicListed: listed,
		}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "OrganizationSettingChanged", "organization", oc.OrgID, oc.UserID, false,
			map[string]any{"org_id": oc.OrgID, "organization_id": oc.OrgID, "profile": true})
	})
}

func (s *Service) ListSettings(ctx context.Context, orgID uuid.UUID) ([]db.ListOrgSettingsRow, error) {
	return s.q.ListOrgSettings(ctx, toPg(orgID))
}

// SetSetting validates the key against the settings catalog (Rule 23 —
// unknown keys rejected, not silently stored).
func (s *Service) SetSetting(ctx context.Context, oc *OrgCtx, key string, value json.RawMessage) error {
	if !orgSettingKeyAllowed(key) {
		return ErrUnknownSetting
	}
	if !json.Valid(value) {
		return ErrBadValue
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.UpsertOrgSetting(ctx, db.UpsertOrgSettingParams{
			OrganizationID: toPg(oc.OrgID), Key: key, Value: value,
		}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "OrganizationSettingChanged", "organization", oc.OrgID, oc.UserID, false,
			map[string]any{"org_id": oc.OrgID, "organization_id": oc.OrgID, "key": key})
	})
}

type Invitation struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Phone     string    `json:"phone"`
	Category  string    `json:"category"`
	Token     uuid.UUID `json:"token,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
	CreatedAt time.Time `json:"created_at"`
}

func (s *Service) CreateInvitation(ctx context.Context, oc *OrgCtx, email, phone, hint, category string) (*Invitation, error) {
	if email == "" && phone == "" {
		return nil, ErrInviteNoContact
	}
	if !memberCategories[category] {
		return nil, ErrBadCategory
	}
	if email != "" {
		// normalize "Name <a@b>" → "a@b" so the accept-time email binding
		// (EqualFold against the user's stored address) can actually match;
		// an unparseable address would make the invite un-acceptable.
		addr, err := mail.ParseAddress(email)
		if err != nil || addr.Address == "" {
			return nil, ErrBadValue
		}
		email = addr.Address
	}
	var out Invitation
	err := s.withTx(ctx, func(q *db.Queries) error {
		r, err := q.CreateInvitation(ctx, db.CreateInvitationParams{
			OrganizationID: toPg(oc.OrgID), Email: pgText(email), Phone: pgText(phone),
			MemberNoHint: pgText(hint), Category: category,
			InvitedBy: toPg(oc.MembershipID), Token: toPg(uuid.New()),
		})
		if err != nil {
			return err
		}
		out = Invitation{ID: fromPg(r.ID), Email: email, Phone: phone, Category: category,
			Token: fromPg(r.Token), ExpiresAt: r.ExpiresAt.Time}
		// membership_id is the inviter's (no invitee membership exists yet);
		// invitation_id identifies the invite itself.
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MemberInvited", "invitation", fromPg(r.ID), oc.UserID, true,
			map[string]any{"membership_id": oc.MembershipID, "invitation_id": fromPg(r.ID),
				"org_id": oc.OrgID, "email": email})
	})
	return &out, err
}

func (s *Service) ListInvitations(ctx context.Context, orgID uuid.UUID) ([]Invitation, error) {
	rows, err := s.q.ListPendingInvitations(ctx, toPg(orgID))
	if err != nil {
		return nil, err
	}
	out := make([]Invitation, len(rows))
	for i, r := range rows {
		out[i] = Invitation{ID: fromPg(r.ID), Email: r.Email.String, Phone: r.Phone.String,
			Category: r.Category, ExpiresAt: r.ExpiresAt.Time, CreatedAt: r.CreatedAt.Time}
	}
	return out, nil
}

// RevokeInvitation is org-scoped — an invitation id grants nothing outside
// the caller's org. Only pending invites flip; anything else is 404.
func (s *Service) RevokeInvitation(ctx context.Context, oc *OrgCtx, inviteID uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.SetInvitationStatus(ctx, db.SetInvitationStatusParams{
			ID: toPg(inviteID), OrganizationID: toPg(oc.OrgID), Status: "revoked",
		}); err != nil {
			if err == pgx.ErrNoRows {
				return ErrInviteNotFound
			}
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "InvitationRevoked", "invitation", inviteID, oc.UserID, false,
			map[string]any{"invitation_id": inviteID, "org_id": oc.OrgID})
	})
}

// AcceptInvitation — public token flow: creates the membership for the
// authenticated user (invited→applied fast-path, §10).
func (s *Service) AcceptInvitation(ctx context.Context, userID uuid.UUID, token uuid.UUID) (uuid.UUID, error) {
	inv, err := s.q.GetInvitationByToken(ctx, toPg(token))
	if err != nil || inv.Status != "pending" || inv.ExpiresAt.Time.Before(time.Now()) {
		return uuid.Nil, ErrInviteDead
	}
	// email-targeted invites bind to the acceptor's verified address
	if inv.Email.Valid && inv.Email.String != "" {
		u, err := s.q.GetUserByID(ctx, toPg(userID))
		if err != nil || !strings.EqualFold(u.Email, inv.Email.String) {
			return uuid.Nil, ErrForbidden
		}
	}
	orgID := fromPg(inv.OrganizationID)
	var tenantID pgtype.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		org, err := q.GetOrg(ctx, inv.OrganizationID)
		if err != nil {
			if err == pgx.ErrNoRows {
				return ErrInviteDead // org soft-deleted — invite dies with it
			}
			return err
		}
		tenantID = org.TenantID
		if _, err := q.SetInvitationStatus(ctx, db.SetInvitationStatusParams{
			ID: inv.ID, OrganizationID: inv.OrganizationID, Status: "accepted",
		}); err != nil {
			if err == pgx.ErrNoRows {
				return ErrInviteDead // flipped by a concurrent accept
			}
			return err
		}
		// Idempotent: an existing membership means the goal is already
		// achieved (re-accept, prior approval) — consume the invite and
		// move on instead of failing on the unique constraint.
		if _, err := q.GetMembership(ctx, db.GetMembershipParams{
			OrganizationID: inv.OrganizationID, UserID: toPg(userID),
		}); err != nil {
			if err != pgx.ErrNoRows {
				return err
			}
			if _, err := q.CreateMembership(ctx, db.CreateMembershipParams{
				OrganizationID: inv.OrganizationID, UserID: toPg(userID),
				MemberNo: inv.MemberNoHint, Category: inv.Category, LifecycleState: "applied",
			}); err != nil {
				return err
			}
			// A departed inviter shouldn't block acceptance — granted_by
			// falls back to NULL (invited_by points at a membership that may
			// no longer exist, e.g. expelled/resigned).
			var grantedBy pgtype.UUID
			if mb, err := q.GetMembershipByID(ctx, inv.InvitedBy); err == nil {
				grantedBy = mb.UserID
			} else if err != pgx.ErrNoRows {
				return err
			}
			if err := q.AssignRoleBySlug(ctx, db.AssignRoleBySlugParams{
				UserID: toPg(userID), TenantID: tenantID, GrantedBy: grantedBy, Slug: "member",
			}); err != nil {
				return err
			}
		}
		return events.Emit(ctx, q, fromPg(tenantID), orgID, "InvitationAccepted", "invitation", fromPg(inv.ID), userID, true,
			map[string]any{"invitation_id": fromPg(inv.ID), "org_id": orgID, "user_id": userID})
	})
	s.InvalidateOrgCtx(ctx, userID, orgID)
	s.InvalidatePermCache(ctx, userID, orgID) // Rule 6: role grant on accept
	return orgID, err
}

type Branding struct {
	PrimaryColor   string          `json:"primary_color"`
	SecondaryColor string          `json:"secondary_color"`
	Motto          string          `json:"motto"`
	FoundedYear    int32           `json:"founded_year"`
	PortalConfig   json.RawMessage `json:"portal_config"`
}

func (s *Service) SetBranding(ctx context.Context, oc *OrgCtx, b Branding) error {
	if b.PortalConfig == nil {
		b.PortalConfig = json.RawMessage("{}")
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.UpsertBranding(ctx, db.UpsertBrandingParams{
			OrganizationID: toPg(oc.OrgID), PrimaryColor: pgText(b.PrimaryColor),
			SecondaryColor: pgText(b.SecondaryColor), Motto: pgText(b.Motto),
			FoundedYear:  pgtype.Int4{Int32: b.FoundedYear, Valid: b.FoundedYear != 0},
			PortalConfig: b.PortalConfig,
		}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "OrgBrandingUpdated", "organization", oc.OrgID, oc.UserID, false,
			map[string]any{"org_id": oc.OrgID})
	})
}

func (s *Service) GetBranding(ctx context.Context, orgID uuid.UUID) (*Branding, error) {
	r, err := s.q.GetBranding(ctx, toPg(orgID))
	if err != nil {
		if err == pgx.ErrNoRows {
			return &Branding{}, nil
		}
		return nil, err
	}
	return &Branding{PrimaryColor: r.PrimaryColor.String, SecondaryColor: r.SecondaryColor.String,
		Motto: r.Motto.String, FoundedYear: r.FoundedYear.Int32, PortalConfig: r.PortalConfig}, nil
}

func (s *Service) Directory(ctx context.Context) ([]db.ListPublicOrgsRow, error) {
	return s.q.ListPublicOrgs(ctx)
}
