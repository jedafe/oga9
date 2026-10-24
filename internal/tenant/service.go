// Ref: oga9-engineering/sections/04-tenancy-organizations.md, 10-organization-core.md
// Tenancy + organization domain service. Org context resolves server-side
// from verified membership — never from request parameters alone (Rule 24).
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
	ErrTenantRequired   = errors.New("tenant_required")
	ErrNoTenant         = errors.New("no_administered_tenant")
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
func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: !t.IsZero()} }

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

// HasPermission moved to rbac.go (org-scoped resolver, Phase 04).

// HasTenantRole reports whether the user holds the named role in the tenant —
// used by tenant resolution where org context doesn't exist yet.
func (s *Service) HasTenantRole(ctx context.Context, userID, tenantID uuid.UUID, slug string) bool {
	admins, err := s.q.ListOrgAdminTenants(ctx, toPg(userID))
	if err != nil {
		return false
	}
	for _, t := range admins {
		if fromPg(t) == tenantID {
			return slug == "org_admin" // the only tenant-role check in use
		}
	}
	return false
}

// UpdateOrgProfile moved to ops.go

// emit appends an outbox event inside the org's mutation tx (Rule 12).
// `sourced` marks ADR-004 replayable events per the domain-events catalog;
// documented payload keys are supplied by each call site (actor_id and
// aggregate_id are injected automatically).
func emit(ctx context.Context, q *db.Queries, tenantID, orgID uuid.UUID, name, aggType string, aggID, actorID uuid.UUID, sourced bool, payload map[string]any) error {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["actor_id"] = actorID
	payload["aggregate_id"] = aggID
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return q.EmitDomainEvent(ctx, db.EmitDomainEventParams{
		EventType: name, AggregateType: aggType, AggregateID: toPg(aggID),
		TenantID: toPg(tenantID), OrganizationID: toPg(orgID), ActorID: toPg(actorID),
		Payload: b, Sourced: sourced,
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
	// level_labels are org-defined tier names for hq depth — descriptive
	// metadata for the unit tree UI, not units to create.
	LevelLabels []string `json:"level_labels"`
	FieldGroups []struct {
		Slug       string `json:"slug"`
		Name       string `json:"name"`
		EntityType string `json:"entity_type"`
		Fields     []struct {
			Key      string `json:"key"`
			Label    string `json:"label"`
			Kind     string `json:"kind"`
			Required bool   `json:"required"`
		} `json:"fields"`
	} `json:"field_groups"`
	// positions/committees/contribution_types instantiate with their owning
	// modules (Phases 04/09/12) — the org keeps template_slug for backfill.
}

// resolveOrgTenant picks the tenant the org lands under — server-side from
// the caller's org_admin role assignments (Rule 24: a request-supplied
// tenant_id is honoured only after verifying the caller administers it).
// Returns provision=true when the caller administers nothing yet and a
// tenant must be created inside the org's tx.
func (s *Service) resolveOrgTenant(ctx context.Context, userID, requested uuid.UUID) (uuid.UUID, bool, error) {
	if requested != uuid.Nil {
		if !s.HasTenantRole(ctx, userID, requested, "org_admin") {
			return uuid.Nil, false, ErrNoTenant // no admin rights there (or tenant unknown)
		}
		if _, err := s.q.GetTenant(ctx, toPg(requested)); err != nil {
			return uuid.Nil, false, ErrNoTenant
		}
		return requested, false, nil
	}
	admins, err := s.q.ListOrgAdminTenants(ctx, toPg(userID))
	if err != nil {
		return uuid.Nil, false, err
	}
	switch len(admins) {
	case 0:
		return uuid.Nil, true, nil // first org → provision the tenant
	case 1:
		return fromPg(admins[0]), false, nil
	default:
		return uuid.Nil, false, ErrTenantRequired // ambiguous — send tenant_id
	}
}

// CreateOrg creates an organization under the caller's administered tenant
// (provisioning one on first create), then membership + org_admin role +
// template instantiation in one tx (§10: creator becomes first member).
// limit.organizations is intentionally unenforced until the Phase 07
// entitlement service lands.
func (s *Service) CreateOrg(ctx context.Context, userID uuid.UUID, name, slug, templateSlug, orgType, country, city, billingEmail string, requestedTenant uuid.UUID) (*Org, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug == "" || len(name) == 0 {
		return nil, ErrBadValue
	}
	tenantID, provision, err := s.resolveOrgTenant(ctx, userID, requestedTenant)
	if err != nil {
		return nil, err
	}
	if provision {
		addr, err := mail.ParseAddress(billingEmail)
		if err != nil {
			return nil, ErrBadValue
		}
		billingEmail = addr.Address // normalize "Name <a@b>" like registration
	}
	var seed templateSeed
	var orgID uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		ts, err := q.GetTemplate(ctx, templateSlug)
		if err != nil {
			return ErrTemplateNotFound
		}
		_ = json.Unmarshal(ts, &seed)

		if provision {
			tid, err := q.CreateTenant(ctx, db.CreateTenantParams{
				Name: name, Slug: slug + "-" + uuid.NewString()[:8], BillingEmail: billingEmail,
			})
			if err != nil {
				return err
			}
			tenantID = fromPg(tid)
			// clone the system role templates into the new tenant
			if err := q.CloneTenantRoles(ctx, tid); err != nil {
				return err
			}
			if err := q.CloneTenantRolePermissions(ctx, tid); err != nil {
				return err
			}
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
		if provision {
			if err := q.SetTenantOwnerOrg(ctx, db.SetTenantOwnerOrgParams{ID: toPg(tenantID), OwnerOrgID: toPg(orgID)}); err != nil {
				return err
			}
		}
		if _, err := q.CreateMembership(ctx, db.CreateMembershipParams{
			OrganizationID: toPg(orgID), UserID: toPg(userID),
			Category: "executive", LifecycleState: "active",
		}); err != nil {
			return err
		}
		if err := q.AssignRoleBySlug(ctx, db.AssignRoleBySlugParams{
			UserID: toPg(userID), TenantID: toPg(tenantID), GrantedBy: toPg(userID), Slug: "org_admin",
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
		// template-seeded custom field groups/fields (ADR-022, source='template')
		for _, g := range seed.FieldGroups {
			gid, err := q.CreateCustomFieldGroup(ctx, db.CreateCustomFieldGroupParams{
				OrganizationID: toPg(orgID), EntityType: g.EntityType, Name: g.Name,
				Slug: g.Slug, Source: "template",
			})
			if err != nil {
				return err
			}
			for _, f := range g.Fields {
				if err := q.CreateCustomField(ctx, db.CreateCustomFieldParams{
					GroupID: gid, OrganizationID: toPg(orgID), EntityType: g.EntityType,
					Key: f.Key, Label: f.Label, Kind: f.Kind, Required: f.Required,
					Source: "template",
				}); err != nil {
					return err
				}
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
		if err := emit(ctx, q, tenantID, orgID, "OrganizationCreated", "organization", orgID, userID, true,
			map[string]any{"org_id": orgID, "organization_id": orgID, "tenant_id": tenantID, "template_slug": templateSlug}); err != nil {
			return err
		}
		// positions/committees/contribution_types instantiate with their
		// modules — the deferred list lets Phase 04/09/12 consumers finish
		// instantiation from this event (template_slug is also on the org).
		if err := emit(ctx, q, tenantID, orgID, "TemplateInstantiated", "organization", orgID, userID, false,
			map[string]any{"org_id": orgID, "organization_id": orgID, "template_slug": templateSlug,
				"deferred": []string{"positions", "committees", "contribution_types"}}); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		s.InvalidatePermCache(ctx, userID, orgID) // Rule 6: org_admin grant
		s.InvalidateOrgCtx(ctx, userID, orgID)
	}
	return s.GetOrg(ctx, orgID)
}
