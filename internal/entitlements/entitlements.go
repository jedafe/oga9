// Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
// Central entitlement resolution (Rule 25) — IsEnabled/Limit derive from the
// tenant's plan ∪ self-hosted license. tenant_addons grants merge here when
// the g13 tables land. Results cache at oga9:entitlements:{tenant_id} (10min,
// catalogued); Invalidate on any subscription/plan/license change.
package entitlements

import (
	"context"
	"encoding/json"
	"log"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	valkey "github.com/valkey-io/valkey-go"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
)

const cacheTTL = 600 // seconds — oga9:entitlements:{tenant_id} (10min)

// Set is the merged effective entitlement view for a tenant.
type Set struct {
	Capabilities []string         `json:"capabilities"`
	Limits       map[string]int64 `json:"limits"`
}

type Service struct {
	q  *db.Queries
	vk valkey.Client
}

func NewService(pool *pgxpool.Pool, vk valkey.Client) *Service {
	return &Service{q: db.New(pool), vk: vk}
}

func toPg(u uuid.UUID) pgtype.UUID { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }

func key(tenantID uuid.UUID) string { return "oga9:entitlements:" + tenantID.String() }

// Set returns the merged effective entitlement view (for /me/entitlements).
func (s *Service) Set(ctx context.Context, tenantID uuid.UUID) (*Set, error) {
	return s.resolve(ctx, tenantID)
}

// IsEnabled answers "does this tenant have capability X" (Rule 25 — never a
// plan-name check).
func (s *Service) IsEnabled(ctx context.Context, tenantID uuid.UUID, capability string) bool {
	set, err := s.resolve(ctx, tenantID)
	if err != nil {
		return false
	}
	for _, c := range set.Capabilities {
		if c == capability {
			return true
		}
	}
	return false
}

// Limit returns the merged limit value; -1 means unlimited, 0 means the
// limit is not granted (deny-by-default for unconfigured keys).
func (s *Service) Limit(ctx context.Context, tenantID uuid.UUID, limitKey string) int64 {
	set, err := s.resolve(ctx, tenantID)
	if err != nil {
		return 0
	}
	return set.Limits[limitKey] // missing → 0
}

// CheckLimit evaluates a limit against current usage: allowed=false on
// breach (hard limit → limit_exceeded upstream), warn=true at ≥80% (soft
// threshold → upgrade-path notification per §07). Unlimited (-1) always
// allows.
func (s *Service) CheckLimit(ctx context.Context, tenantID uuid.UUID, limitKey string, used int64) (allowed, warn bool) {
	l := s.Limit(ctx, tenantID, limitKey)
	if l < 0 {
		return true, false
	}
	if used >= l {
		return false, false
	}
	return true, l > 0 && used*100 >= l*80
}

// Invalidate drops the cached set — call after subscription/plan/license/
// addon changes (Rule 25).
func (s *Service) Invalidate(ctx context.Context, tenantID uuid.UUID) {
	if err := s.vk.Do(ctx, s.vk.B().Del().Key(key(tenantID)).Build()).Error(); err != nil {
		log.Printf("entitlement cache invalidate failed (%s): %v", tenantID, err)
	}
}

// InvalidateConsumer is an outbox consumer (§05 contract): on
// SubscriptionChanged / AppActivated / AppDeactivated / LicenseIssued it
// drops the tenant's cached set. Idempotent — DEL is safe to replay.
func (s *Service) InvalidateConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	switch e.Type {
	case "SubscriptionChanged", "AppActivated", "AppDeactivated", "LicenseIssued", "LicenseRenewed":
		s.Invalidate(ctx, e.TenantID)
		return events.Emit(ctx, q, e.TenantID, uuid.Nil, "EntitlementsRefreshed",
			"entitlements", e.TenantID, e.ActorID, false,
			map[string]any{"tenant_id": e.TenantID})
	}
	return nil
}

// resolve builds the merged set (plan ∪ license) with a starter-plan floor
// when no subscription row exists yet.
func (s *Service) resolve(ctx context.Context, tenantID uuid.UUID) (*Set, error) {
	if raw, err := s.vk.Do(ctx, s.vk.B().Get().Key(key(tenantID)).Build()).AsBytes(); err == nil {
		var set Set
		if json.Unmarshal(raw, &set) == nil {
			return &set, nil
		}
	}
	planSlug := "starter" // floor for pre-Phase-07 tenants
	if sub, err := s.q.GetSubscription(ctx, toPg(tenantID)); err == nil {
		planSlug = sub.PlanSlug
		switch sub.State {
		case "restricted", "cancelled":
			// Rule 28: lapsed/cancelled tenants drop to the free core —
			// records stay reachable, paid capabilities stop resolving.
			planSlug = "starter"
		}
	} else if err != pgx.ErrNoRows {
		return nil, err
	}
	caps := map[string]bool{}
	limits := map[string]int64{}
	pc, err := s.q.GetPlanCapabilities(ctx, planSlug)
	if err != nil {
		return nil, err
	}
	for _, c := range pc {
		caps[c] = true
	}
	pl, err := s.q.GetPlanLimits(ctx, planSlug)
	if err != nil {
		return nil, err
	}
	for _, l := range pl {
		limits[l.LimitKey] = l.LimitValue
	}
	// License merge (self-hosted, Rule 29): capabilities union; limits take
	// the greater grant across sources. -1 (unlimited) is absorbing — a
	// license that lifts a cap must beat any positive plan limit.
	if lic, err := s.q.GetLatestLicense(ctx, toPg(tenantID)); err == nil {
		var le struct {
			Capabilities []string         `json:"capabilities"`
			Limits       map[string]int64 `json:"limits"`
		}
		if json.Unmarshal(lic.Entitlements, &le) == nil {
			for _, c := range le.Capabilities {
				caps[c] = true
			}
			for k, v := range le.Limits {
				if cur, ok := limits[k]; !ok || (cur != -1 && (v == -1 || v > cur)) {
					limits[k] = v
				}
			}
		}
	}
	set := &Set{Limits: limits}
	for c := range caps {
		set.Capabilities = append(set.Capabilities, c)
	}
	if b, err := json.Marshal(set); err == nil {
		s.vk.Do(ctx, s.vk.B().Set().Key(key(tenantID)).Value(string(b)).ExSeconds(cacheTTL).Build())
	}
	return set, nil
}
