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

	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
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
	ErrNotFound         = errors.New("not_found")
	ErrTermConflict     = errors.New("term_conflict")
	ErrBadBillingEmail  = errors.New("invalid_billing_email")
	ErrBadScopeUnit     = errors.New("invalid_scope_unit")
	ErrDuplicateMember  = errors.New("member_already_seated")
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

// lifecycleBlocked — ADR-014 states that revoke org context entirely.
var lifecycleBlocked = map[string]bool{
	"suspended": true, "expelled": true, "resigned": true, "deceased": true,
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
	// ADR-014 terminal states carry no org context — a suspended/expelled/
	// resigned/deceased member loses access here, not just in UI. Transitional
	// states (applied→registered) still resolve; Phase 08 refines per-state
	// capabilities. Callers MUST InvalidateOrgCtx on lifecycle transitions.
	if lifecycleBlocked[m.LifecycleState] {
		return nil, ErrNotMember
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
// AdminTenants returns the tenants where the user holds org_admin — the
// server-side basis for billing-tenant resolution (Rule 24).
func (s *Service) AdminTenants(ctx context.Context, userID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := s.q.ListOrgAdminTenants(ctx, toPg(userID))
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, uuid.UUID(r.Bytes))
	}
	return out, nil
}

// TenantsWithNode returns the tenants where the caller holds a role granting
// the permission node — the api-routes-catalog gate for /billing/* is the
// org.admin.billing node, not a role name (Rule 20).
func (s *Service) TenantsWithNode(ctx context.Context, userID uuid.UUID, node string) ([]uuid.UUID, error) {
	rows, err := s.q.ListTenantsWithNode(ctx, db.ListTenantsWithNodeParams{
		UserID: toPg(userID), Node: node,
	})
	if err != nil {
		return nil, err
	}
	out := make([]uuid.UUID, 0, len(rows))
	for _, r := range rows {
		out = append(out, uuid.UUID(r.Bytes))
	}
	return out, nil
}

// IsAppActive reports whether appID is installed+active for the tenant —
// the §06 "deactivation disables functionality" check consumed by
// middleware.RequireAppActive on module routes.
func (s *Service) IsAppActive(ctx context.Context, tenantID uuid.UUID, appID string) (bool, error) {
	return s.q.IsAppActive(ctx, db.IsAppActiveParams{TenantID: toPg(tenantID), AppID: appID})
}

func (s *Service) HasTenantRole(ctx context.Context, userID, tenantID uuid.UUID, slug string) bool {
	ok, err := s.q.HasRoleInTenant(ctx, db.HasRoleInTenantParams{
		UserID: toPg(userID), TenantID: toPg(tenantID), Slug: slug,
	})
	return err == nil && ok
}

// UpdateOrgProfile moved to ops.go

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

// positionSeed accepts either "slug" or {"slug","executive"} seed entries —
// a bare slug is an officer-slate position (executive defaults true).
type positionSeed struct {
	Slug      string
	Executive bool
}

func (p *positionSeed) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		if err := json.Unmarshal(b, &p.Slug); err != nil {
			return err
		}
		p.Executive = true
		return nil
	}
	var o struct {
		Slug      string `json:"slug"`
		Executive *bool  `json:"executive"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return err
	}
	p.Slug, p.Executive = o.Slug, o.Executive == nil || *o.Executive
	return nil
}

type templateSeed struct {
	Rules map[string]json.RawMessage `json:"rules"`
	Theme map[string]any             `json:"theme"`
	// Template officer positions + committees instantiate at org create
	// (§10 test scenario 1); contribution_types wait for Phase 09 finance.
	Positions  []positionSeed `json:"positions"`
	Committees []string       `json:"committees"`
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
	if len(name) == 0 {
		return nil, ErrBadValue
	}
	if !validSlug(slug) {
		return nil, ErrBadSlug
	}
	if orgType == "" {
		orgType = templateSlug // the template defines the org's type taxonomy
	} else if orgType != templateSlug {
		if exists, err := s.q.TemplateExists(ctx, orgType); err != nil || !exists {
			return nil, ErrBadOrgType
		}
	}
	tenantID, provision, err := s.resolveOrgTenant(ctx, userID, requestedTenant)
	if err != nil {
		return nil, err
	}
	if provision {
		addr, err := mail.ParseAddress(billingEmail)
		if err != nil {
			return nil, ErrBadBillingEmail
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
			// platform-scoped event (no org yet); starter trial lands below.
			if err := events.Emit(ctx, q, tenantID, uuid.Nil, "TenantProvisioned", "tenant", tenantID, userID, true,
				map[string]any{"tenant_id": tenantID, "plan_slug": "starter"}); err != nil {
				return err
			}
			// clone the system role templates into the new tenant
			if err := q.CloneTenantRoles(ctx, tid); err != nil {
				return err
			}
			if err := q.CloneTenantRolePermissions(ctx, tid); err != nil {
				return err
			}
			// §07: every tenant starts on a starter-plan trial (the
			// subscription is the plan source of truth — tenants carry no
			// plan column). §06: MVP apps install at provision.
			trialDays := int64(14)
			if v, err := q.GetSystemSetting(ctx, "billing.trial_days"); err == nil {
				var n json.Number
				if json.Unmarshal(v, &n) == nil {
					if i, err := n.Int64(); err == nil {
						trialDays = i
					}
				}
			}
			if err := billing.ProvisionTrial(ctx, q, tenantID, userID, trialDays); err != nil {
				return err
			}
			if err := provisionMVPApps(ctx, q, tenantID); err != nil {
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
		// Template instantiation (§10): positions + committees land now —
		// contribution_types defer to their module phase (the deferred list
		// lets the owning consumers finish from this event; template_slug
		// is also on the org for backfill).
		for _, ps := range seed.Positions {
			pid, err := q.CreatePosition(ctx, db.CreatePositionParams{
				OrganizationID: toPg(orgID), Name: humanizeSlug(ps.Slug), Slug: ps.Slug, IsExecutive: ps.Executive, Seats: 1,
			})
			if err != nil {
				return err
			}
			if err := events.Emit(ctx, q, tenantID, orgID, "PositionCreated", "org_position", fromPg(pid), userID, true,
				map[string]any{"position_id": fromPg(pid), "org_id": orgID, "name": humanizeSlug(ps.Slug), "source": "template"}); err != nil {
				return err
			}
		}
		for _, cs := range seed.Committees {
			uid, err := q.CreateUnit(ctx, db.CreateUnitParams{
				OrganizationID: toPg(orgID), Type: "committee", Name: humanizeSlug(cs), Slug: cs,
			})
			if err != nil {
				return err
			}
			if err := events.Emit(ctx, q, tenantID, orgID, "OrgUnitCreated", "org_unit", fromPg(uid), userID, true,
				map[string]any{"unit_id": fromPg(uid), "org_id": orgID, "organization_id": orgID, "type": "committee", "source": "template"}); err != nil {
				return err
			}
			if err := events.Emit(ctx, q, tenantID, orgID, "CommitteeCreated", "org_unit", fromPg(uid), userID, true,
				map[string]any{"unit_id": fromPg(uid), "org_id": orgID}); err != nil {
				return err
			}
		}
		if err := events.Emit(ctx, q, tenantID, orgID, "OrganizationCreated", "organization", orgID, userID, true,
			map[string]any{"org_id": orgID, "organization_id": orgID, "tenant_id": tenantID, "template_slug": templateSlug}); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, tenantID, orgID, "TemplateInstantiated", "organization", orgID, userID, false,
			map[string]any{"org_id": orgID, "organization_id": orgID, "template_slug": templateSlug,
				"deferred": []string{"contribution_types"}}); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err // tx rolled back — orgID is nil; don't mask the real error
	}
	s.InvalidatePermCache(ctx, userID, orgID) // Rule 6: org_admin grant
	s.InvalidateOrgCtx(ctx, userID, orgID)
	return s.GetOrg(ctx, orgID)
}
