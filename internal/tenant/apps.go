// Ref: oga9-engineering/sections/06-app-framework-registry.md
// App registry + per-tenant installation lifecycle. Installations are
// tenant-scoped; org routes resolve the org's tenant via OrgCtx. Deactivate
// blocks on active dependents and never deletes records (Rule 26).
package tenant

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/entitlements"
	"github.com/jedafe/oga9/internal/events"
)

var ErrAppNotFound = errors.New("app_not_found")
var ErrAppDepsMissing = errors.New("app_dependencies_missing")
var ErrAppHasDependents = errors.New("app_has_dependents")
var ErrAppRequired = errors.New("app_required")
var ErrNotEntitled = errors.New("not_entitled")

// appManifest is the §06 contract — requires drives the dependency graph,
// entitlements.feature is the Rule 25 capability key checked at activate.
type appManifest struct {
	Requires     []string `json:"requires"`
	Entitlements struct {
		Feature string `json:"feature"`
	} `json:"entitlements"`
}

func manifestOf(m json.RawMessage) appManifest {
	var mm appManifest
	_ = json.Unmarshal(m, &mm)
	return mm
}

// ListApps returns the full registry (platform.apps.view surface).
func (s *Service) ListApps(ctx context.Context) ([]db.ListAppsRow, error) {
	return s.q.ListApps(ctx)
}

// OrgApp pairs a registry row with the org tenant's install status.
type OrgApp struct {
	AppID       string `json:"app_id"`
	Label       string `json:"label"`
	Version     string `json:"version"`
	MVPRequired bool   `json:"mvp_required"`
	Status      string `json:"status"` // active|deactivated|not_installed
}

// OrgApps joins the registry with the org tenant's installations.
func (s *Service) OrgApps(ctx context.Context, oc *OrgCtx) ([]OrgApp, error) {
	reg, err := s.q.ListApps(ctx)
	if err != nil {
		return nil, err
	}
	inst, err := s.q.ListInstallations(ctx, toPg(oc.TenantID))
	if err != nil {
		return nil, err
	}
	status := map[string]string{}
	for _, i := range inst {
		status[i.AppID] = i.Status
	}
	out := make([]OrgApp, 0, len(reg))
	for _, a := range reg {
		st := status[a.AppID]
		if st == "" {
			st = "not_installed"
		}
		out = append(out, OrgApp{AppID: a.AppID, Label: a.Label, Version: a.Version,
			MVPRequired: a.MvpRequired, Status: st})
	}
	return out, nil
}

// ActivateApp: dependency closure must already be active (§06), and the
// manifest's feature capability must pass the entitlement check (Rule 25).
// Re-activation is idempotent (upsert).
func (s *Service) ActivateApp(ctx context.Context, oc *OrgCtx, ent *entitlements.Service, appID string) error {
	row, err := s.q.GetApp(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAppNotFound
	}
	if err != nil {
		return err
	}
	m := manifestOf(row.Manifest)
	if m.Entitlements.Feature != "" && !ent.IsEnabled(ctx, oc.TenantID, m.Entitlements.Feature) {
		return ErrNotEntitled
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		// Dependency closure is re-checked inside the tx so a concurrent
		// deactivate can't slip between check and upsert.
		active, err := q.ListActiveApps(ctx, toPg(oc.TenantID))
		if err != nil {
			return err
		}
		activeSet := map[string]bool{}
		for _, a := range active {
			activeSet[a] = true
		}
		for _, req := range m.Requires {
			if !activeSet[req] {
				return ErrAppDepsMissing
			}
		}
		if activeSet[appID] {
			return nil // already active — idempotent no-op, no event
		}
		iid, err := q.ActivateApp(ctx, db.ActivateAppParams{TenantID: toPg(oc.TenantID), AppID: appID})
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AppActivated", "app_installation", uuid.UUID(iid.Bytes), oc.UserID, true,
			map[string]any{"tenant_id": oc.TenantID, "app_id": appID, "org_id": oc.OrgID})
	})
}

// DeactivateApp rejects while an ACTIVE app depends on this one (§06).
// Deactivation disables functionality — records stay (Rule 26).
func (s *Service) DeactivateApp(ctx context.Context, oc *OrgCtx, appID string) error {
	row, err := s.q.GetApp(ctx, appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrAppNotFound
	}
	if err != nil {
		return err
	}
	if row.MvpRequired {
		return ErrAppRequired
	}
	active, err := s.q.ListActiveApps(ctx, toPg(oc.TenantID))
	if err != nil {
		return err
	}
	inSet := false
	for _, other := range active {
		if other == appID {
			inSet = true
			continue
		}
		otherRow, err := s.q.GetApp(ctx, other)
		if err != nil {
			continue
		}
		for _, req := range manifestOf(otherRow.Manifest).Requires {
			if req == appID {
				return ErrAppHasDependents
			}
		}
	}
	if !inSet {
		return nil // never installed or already deactivated — idempotent no-op, no event
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		iid, err := q.DeactivateApp(ctx, db.DeactivateAppParams{TenantID: toPg(oc.TenantID), AppID: appID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // concurrent deactivate won the race — idempotent no-op
		}
		if err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AppDeactivated", "app_installation", uuid.UUID(iid.Bytes), oc.UserID, true,
			map[string]any{"tenant_id": oc.TenantID, "app_id": appID, "org_id": oc.OrgID})
	})
}

// provisionMVPApps installs every mvp_required app on a fresh tenant — runs
// inside the provisioning tx (CreateOrg).
func provisionMVPApps(ctx context.Context, q *db.Queries, tenantID uuid.UUID) error {
	reg, err := q.ListApps(ctx)
	if err != nil {
		return err
	}
	for _, a := range reg {
		if !a.MvpRequired {
			continue
		}
		if _, err := q.ActivateApp(ctx, db.ActivateAppParams{TenantID: toPg(tenantID), AppID: a.AppID}); err != nil {
			return err
		}
	}
	return nil
}
