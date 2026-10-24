// Ref: oga9-engineering/sections/04-tenancy-organizations.md, 10-organization-core.md
// Tenancy + organization domain service. Org context resolves server-side
// from verified membership — never from request parameters alone (Rule 24).
package tenant

import (
	"context"
	"encoding/json"
	"errors"
	"net/mail"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
)

var (
	ErrNotMember        = errors.New("not_a_member")
	ErrForbidden        = errors.New("forbidden")
	ErrOrgNotFound      = errors.New("org_not_found")
	ErrUnitNotFound     = errors.New("unit_not_found")
	ErrUnitDependents   = errors.New("unit_has_dependents")
	ErrUnitCycle        = errors.New("unit_cycle")
	ErrBadUnitType      = errors.New("invalid_unit_type")
	ErrUnknownSetting   = errors.New("unknown_setting_key")
	ErrInviteNotFound   = errors.New("invitation_not_found")
	ErrInviteDead       = errors.New("invitation_expired")
	ErrInviteNoContact  = errors.New("contact_required")
	ErrTemplateNotFound = errors.New("template_not_found")
	ErrSlugTaken        = errors.New("slug_taken")
	ErrLimitExceeded    = errors.New("limit_exceeded")
)

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	vk   valkey.Client
}

func NewService(pool *pgxpool.Pool, vk valkey.Client) *Service {
	return &Service{pool: pool, q: db.New(pool), vk: vk}
}

func toPg(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: true} }
func fromPg(p pgtype.UUID) uuid.UUID {
	u, _ := uuid.FromBytes(p.Bytes[:])
	return u
}
func pgText(s string) pgtype.Text { return pgtype.Text{String: s, Valid: s != ""} }
func pgUUIDPtr(u uuid.UUID) pgtype.UUID {
	return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil}
}

// OrgCtx is the verified (user, org, membership) context injected into
// request scope by ResolveOrgCtx (Rule 24).
type OrgCtx struct {
	OrgID        uuid.UUID `json:"org_id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	UserID       uuid.UUID `json:"user_id"`
	MembershipID uuid.UUID `json:"membership_id"`
	Category     string    `json:"category"`
	State        string    `json:"lifecycle_state"`
}

// ResolveOrgCtx verifies the caller's membership for orgID, injecting a
// cached tenant_ctx (oga9:tenant_ctx:{user}:{org}, 5 min).
func (s *Service) ResolveOrgCtx(ctx context.Context, userID, orgID uuid.UUID) (*OrgCtx, error) {
	key := "oga9:tenant_ctx:" + userID.String() + ":" + orgID.String()
	if raw, err := s.vk.Do(ctx, s.vk.B().Get().Key(key).Build()).AsBytes(); err == nil {
		var oc OrgCtx
		if json.Unmarshal(raw, &oc) == nil && oc.MembershipID != uuid.Nil {
			return &oc, nil
		}
	}
	org, err := s.q.GetOrg(ctx, toPg(orgID))
	if err == pgx.ErrNoRows {
		return nil, ErrOrgNotFound
	}
	if err != nil {
		return nil, err
	}
	m, err := s.q.GetMembership(ctx, db.GetMembershipParams{OrganizationID: toPg(orgID), UserID: toPg(userID)})
	if err == pgx.ErrNoRows {
		return nil, ErrNotMember
	}
	if err != nil {
		return nil, err
	}
	oc := &OrgCtx{
		OrgID: orgID, TenantID: fromPg(org.TenantID), UserID: userID,
		MembershipID: fromPg(m.ID), Category: m.Category, State: m.LifecycleState,
	}
	if b, err := json.Marshal(oc); err == nil {
		_ = s.vk.Do(ctx, s.vk.B().Set().Key(key).Value(string(b)).ExSeconds(300).Build()).Error()
	}
	return oc, nil
}

// InvalidateOrgCtx drops the cached tenant_ctx (membership/scope changes).
func (s *Service) InvalidateOrgCtx(ctx context.Context, userID, orgID uuid.UUID) {
	_ = s.vk.Do(ctx, s.vk.B().Del().Key("oga9:tenant_ctx:"+userID.String()+":"+orgID.String()).Build())
}

// HasPermission resolves user→tenant role→permission node (Rule 20: nodes,
// never position names).
func (s *Service) HasPermission(ctx context.Context, userID, tenantID uuid.UUID, node string) bool {
	ok, err := s.q.HasPermission(ctx, db.HasPermissionParams{
		UserID: toPg(userID), TenantID: toPg(tenantID), Node: node,
	})
	return err == nil && ok
}

// UpdateOrgProfile moved to ops.go

// emit appends an outbox event inside the org's mutation tx (Rule 12).
func emit(ctx context.Context, q *db.Queries, tenantID uuid.UUID, name, aggType string, aggID, actorID uuid.UUID, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["actor_id"] = actorID
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return q.EmitDomainEvent(ctx, db.EmitDomainEventParams{
		Name: name, AggregateType: aggType, AggregateID: toPg(aggID),
		TenantID: toPg(tenantID), ActorID: toPg(actorID), Payload: b,
	})
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

type Org struct {
	ID           uuid.UUID `json:"id"`
	TenantID     uuid.UUID `json:"tenant_id"`
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	Type         string    `json:"org_type"`
	CountryCode  string    `json:"country_code"`
	City         string    `json:"city"`
	PublicListed bool      `json:"is_public_listed"`
}

func toOrg(r db.GetOrgRow) *Org {
	return &Org{ID: fromPg(r.ID), TenantID: fromPg(r.TenantID), Name: r.Name, Slug: r.Slug,
		Type: r.OrgType, CountryCode: r.CountryCode.String, City: r.City.String,
		PublicListed: r.IsPublicListed}
}

func (s *Service) GetOrg(ctx context.Context, id uuid.UUID) (*Org, error) {
	r, err := s.q.GetOrg(ctx, toPg(id))
	if err == pgx.ErrNoRows {
		return nil, ErrOrgNotFound
	}
	if err != nil {
		return nil, err
	}
	return toOrg(r), nil
}

func (s *Service) ListUserOrgs(ctx context.Context, userID uuid.UUID) ([]db.ListUserOrgsRow, error) {
	return s.q.ListUserOrgs(ctx, toPg(userID))
}

func (s *Service) ListTemplates(ctx context.Context) ([]db.ListTemplatesRow, error) {
	return s.q.ListTemplates(ctx)
}

type templateSeed struct {
	Rules map[string]json.RawMessage `json:"rules"`
	Theme map[string]any             `json:"theme"`
	Units []struct {
		Name       string `json:"name"`
		Type       string `json:"type"`
		LevelLabel string `json:"level_label"`
	} `json:"units"`
	// positions/committees/contribution_types/field_groups land with their
	// modules (Phases 04/08/12) — recorded in seed for the instantiator.
}

// CreateOrg provisions tenant + org + creator membership + org_admin role +
// template instantiation in one tx (§10: creator becomes first member).
func (s *Service) CreateOrg(ctx context.Context, userID uuid.UUID, name, slug, templateSlug, orgType, country, city, billingEmail string) (*Org, error) {
	if _, err := mail.ParseAddress(billingEmail); err != nil {
		return nil, ErrBadValue
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" || len(name) == 0 {
		return nil, ErrBadValue
	}
	var seed templateSeed
	var orgID uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		ts, err := q.GetTemplate(ctx, templateSlug)
		if err != nil {
			return ErrTemplateNotFound
		}
		_ = json.Unmarshal(ts, &seed)

		tid, err := q.CreateTenant(ctx, db.CreateTenantParams{
			Name: name, Slug: slug + "-" + uuid.NewString()[:8], BillingEmail: billingEmail,
		})
		if err != nil {
			return err
		}
		tenantID := fromPg(tid)
		if n, err := q.CountTenantOrgs(ctx, tid); err == nil && n > 0 {
			return ErrLimitExceeded // limit.organizations resolved by entitlements (Phase 07)
		}
		var o db.CreateOrgRow
		o, err = q.CreateOrg(ctx, db.CreateOrgParams{
			TenantID: toPg(tenantID), Name: name, Slug: slug, TemplateSlug: pgText(templateSlug),
			OrgType: orgType, CountryCode: pgText(country), City: pgText(city),
		})
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return ErrSlugTaken
			}
			return err
		}
		orgID = fromPg(o.ID)
		if err := q.SetTenantOwnerOrg(ctx, db.SetTenantOwnerOrgParams{ID: tid, OwnerOrgID: toPg(orgID)}); err != nil {
			return err
		}
		if _, err := q.CreateMembership(ctx, db.CreateMembershipParams{
			OrganizationID: toPg(orgID), UserID: toPg(userID),
			Category: "executive", LifecycleState: "active",
		}); err != nil {
			return err
		}
		if err := q.CloneTenantRoles(ctx, tid); err != nil {
			return err
		}
		if err := q.CloneTenantRolePermissions(ctx, tid); err != nil {
			return err
		}
		if err := q.AssignRoleBySlug(ctx, db.AssignRoleBySlugParams{
			UserID: toPg(userID), TenantID: tid, GrantedBy: toPg(userID), Slug: "org_admin",
		}); err != nil {
			return err
		}
		for k, v := range seed.Rules {
			if err := q.UpsertOrgSetting(ctx, db.UpsertOrgSettingParams{
				OrganizationID: toPg(orgID), Key: k, Value: []byte(v),
			}); err != nil {
				return err
			}
		}
		for i, u := range seed.Units {
			if !map[string]bool{"hq": true, "branch": true, "chapter": true, "committee": true, "wing": true}[u.Type] {
				continue
			}
			if _, err := q.CreateUnit(ctx, db.CreateUnitParams{
				OrganizationID: toPg(orgID), ParentUnitID: pgUUIDPtr(uuid.Nil),
				Type: u.Type, LevelLabel: pgText(u.LevelLabel), Name: u.Name,
				Slug: u.Type + "-" + strconv.Itoa(i),
			}); err != nil {
				return err
			}
		}
		if seed.Theme != nil {
			pc, _ := seed.Theme["primary_color"].(string)
			if err := q.UpsertBranding(ctx, db.UpsertBrandingParams{
				OrganizationID: toPg(orgID), PrimaryColor: pgText(pc), PortalConfig: []byte("{}"),
			}); err != nil {
				return err
			}
		}
		if err := emit(ctx, q, tenantID, "OrganizationCreated", "organization", orgID, userID,
			map[string]any{"organization_id": orgID, "tenant_id": tenantID, "template_slug": templateSlug}); err != nil {
			return err
		}
		if err := emit(ctx, q, tenantID, "TemplateInstantiated", "organization", orgID, userID,
			map[string]any{"organization_id": orgID, "template_slug": templateSlug}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return s.GetOrg(ctx, orgID)
}
