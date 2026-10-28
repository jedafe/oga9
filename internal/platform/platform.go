// Package platform implements §15 subscription & platform admin — the
// control-plane surface: tenant provisioning, plan/subscription overrides,
// deployments, support impersonation, audit + usage reads, feature flags.
//
// Ref: oga9-engineering/sections/15-subscription-admin.md
package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/billing"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/storage"
)

var (
	ErrNotFound            = errors.New("not found")
	ErrBadValue            = errors.New("bad value")
	ErrProvisionConflict   = errors.New("provision_in_progress")
	ErrProvisionMismatch   = errors.New("provision_intent_mismatch")
	ErrSessionExpired      = errors.New("support_session_expired")
	ErrImpersonationDenied = errors.New("impersonation_denied")
)

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }

// provisionPayload serializes the request for the durable row — including
// the server-set RequestedBy, so a sweep-redriven run keeps the original
// operator identity for audit/events. (RequestedBy is still excluded from
// the intent COMPARISON — see sameIntent.)
func provisionPayload(in ProvisionRequest) []byte {
	b, _ := json.Marshal(in)
	return b
}

// sameIntent compares a stored payload against the incoming request.
// jsonb normalizes key order, so compare via unmarshal, not bytes.
func sameIntent(stored []byte, in ProvisionRequest) bool {
	if len(stored) == 0 {
		return true // rows created before the payload column — allow
	}
	var prior ProvisionRequest
	if err := json.Unmarshal(stored, &prior); err != nil {
		return false
	}
	prior.RequestedBy = uuid.Nil
	want := in
	want.RequestedBy = uuid.Nil
	// TrialDays is *int64 — a struct == would compare pointer identity,
	// so identical replays that set trial_days would false-mismatch.
	if (prior.TrialDays == nil) != (want.TrialDays == nil) {
		return false
	}
	if prior.TrialDays != nil && *prior.TrialDays != *want.TrialDays {
		return false
	}
	prior.TrialDays, want.TrialDays = nil, nil
	return prior == want
}

// OrgBootstrap creates the owner org from a template inside the provision
// pipeline — wired to tenant.CreateOrg (Rule 26: platform never writes org
// tables itself).
type OrgBootstrap func(ctx context.Context, userID uuid.UUID, name, slug, templateSlug, orgType, country, city, billingEmail string, requestedTenant uuid.UUID) (uuid.UUID, error)

// TenantScaffolder prepares the new tenant for its first org — role
// templates + permission grants, MVP app installs, and the owner's
// org_admin assignment. Wired to tenant.ScaffoldTenant (Rule 26: role and
// app rows belong to the tenant module).
type TenantScaffolder func(ctx context.Context, q *db.Queries, tenantID, ownerID uuid.UUID) error

type Service struct {
	pool      *pgxpool.Pool
	q         *db.Queries
	boot      OrgBootstrap
	scaffold  TenantScaffolder
	welcomeFn WelcomeMail
	// sessionMinter mints the impersonation JWT — wired to
	// auth.CreateSupportSession (Rule 26: platform never writes
	// user_sessions itself).
	sessionFn func(ctx context.Context, supportSessionID, targetUserID uuid.UUID, ttl time.Duration, ip, ua string) (token string, sessionID uuid.UUID, err error)
	trial     int64             // default trial days — operator may override per request
	store     storage.BlobStore // tenant_exports blob backend (§13.06 async export)
}

// SetStore wires the export blob backend (same uploads dir as documents).
func (s *Service) SetStore(st storage.BlobStore) { s.store = st }

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool), trial: 14}
}

func (s *Service) SetOrgBootstrap(fn OrgBootstrap)         { s.boot = fn }
func (s *Service) SetTenantScaffolder(fn TenantScaffolder) { s.scaffold = fn }
func (s *Service) SetTrialDays(d int64)                    { s.trial = d }

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(db.New(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ProvisionRequest is the operator's POST /platform/tenants body.
type ProvisionRequest struct {
	RequestID    string    `json:"request_id"`
	RequestedBy  uuid.UUID `json:"requested_by"` // operator user (server-set)
	TenantName   string    `json:"tenant_name"`
	TenantSlug   string    `json:"tenant_slug"`
	Edition      string    `json:"edition"`
	BillingEmail string    `json:"billing_email"`
	OwnerEmail   string    `json:"owner_email"`
	OwnerName    string    `json:"owner_name"`
	OrgName      string    `json:"org_name"`
	OrgSlug      string    `json:"org_slug"`
	Template     string    `json:"template"`
	OrgType      string    `json:"org_type"`
	Country      string    `json:"country"`
	City         string    `json:"city"`
	TrialDays    *int64    `json:"trial_days"`
}

// Provision — §15 pipeline: durable request row (idempotency key) →
// tenant + owner user + trial subscription (billing seam) → owner org via
// the tenant bootstrap → TenantProvisioned. Replays return recorded ids;
// a failed run reclaims its row for retry.
func (s *Service) Provision(ctx context.Context, in ProvisionRequest, actor uuid.UUID) (tenantID, orgID uuid.UUID, status string, err error) {
	// Template is required — org_templates has no generic default (the
	// seeded set is union/social-club/alumni/cooperative/professional/
	// religious/cda), so the operator picks one explicitly, matching
	// self-serve POST /orgs.
	if in.RequestID == "" || in.TenantName == "" || in.TenantSlug == "" || in.OrgSlug == "" ||
		in.OwnerName == "" || in.OwnerEmail == "" || in.Template == "" {
		return uuid.Nil, uuid.Nil, "", ErrBadValue
	}
	if in.TrialDays != nil && *in.TrialDays < 0 {
		return uuid.Nil, uuid.Nil, "", ErrBadValue
	}
	if in.Edition == "" {
		in.Edition = "cloud"
	}
	if in.OrgName == "" {
		in.OrgName = in.TenantName
	}
	row, err := s.q.CreateProvision(ctx, db.CreateProvisionParams{RequestID: in.RequestID, Payload: provisionPayload(in)})
	var resumed *db.TenantProvisions
	// ON CONFLICT DO NOTHING RETURNING emits no row → ErrNoRows is the
	// conflict (claim) path, not a real error.
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, "", err
	}
	if !row.ID.Valid {
		existing, e := s.q.GetProvision(ctx, in.RequestID)
		if e != nil {
			return uuid.Nil, uuid.Nil, "", fmt.Errorf("get_provision: %w", e)
		}
		if !sameIntent(existing.Payload, in) {
			return uuid.Nil, uuid.Nil, "", ErrProvisionMismatch
		}
		if existing.Status == "done" {
			return fromPg(existing.TenantID), fromPg(existing.OrganizationID), "done", nil
		}
		// failed or stale-processing → atomically reclaim; a live worker
		// (or concurrent reclaimer) makes the claim fail → conflict.
		claimed, e := s.q.ClaimProvision(ctx, in.RequestID)
		if e == nil && claimed.ID.Valid && !sameIntent(claimed.Payload, in) {
			return uuid.Nil, uuid.Nil, "", ErrProvisionMismatch
		}
		if e != nil || !claimed.ID.Valid {
			if e != nil {
				return uuid.Nil, uuid.Nil, "", fmt.Errorf("claim: %w", e)
			}
			return uuid.Nil, uuid.Nil, "processing", ErrProvisionConflict
		}
		if claimed.TenantID.Valid {
			resumed = &claimed // tenant+user+sub committed last run — resume
		}
		err = nil // conflict resolved via claim — clear the stale ErrNoRows
	}

	var userID uuid.UUID
	if resumed != nil {
		tenantID = fromPg(resumed.TenantID)
		userID = fromPg(resumed.OwnerUserID)
	} else {
		err = s.withTx(ctx, func(q *db.Queries) error {
			t, err := q.CreateTenantRow(ctx, db.CreateTenantRowParams{
				Name: in.TenantName, Slug: in.TenantSlug,
				BillingEmail: in.BillingEmail, Edition: in.Edition})
			if err != nil {
				return err
			}
			tenantID = fromPg(t.ID)
			u, err := q.ProvisionUser(ctx, db.ProvisionUserParams{
				Email: in.OwnerEmail, DisplayName: in.OwnerName})
			if err != nil {
				return err
			}
			userID = fromPg(u.ID)
			days := s.trial
			if in.TrialDays != nil {
				days = *in.TrialDays
			}
			if err := billing.ProvisionTrial(ctx, q, tenantID, actor, days); err != nil {
				return err
			}
			// §15 scaffold: role templates + MVP apps + owner org_admin —
			// without it the bootstrap's administered-tenant gate refuses
			// the org and every provision dies at this stage.
			if s.scaffold != nil {
				if err := s.scaffold(ctx, q, tenantID, userID); err != nil {
					return err
				}
			}
			return nil
		})
	} // end fresh-provision tx
	if err != nil {
		_, _ = s.q.FinishProvision(ctx, db.FinishProvisionParams{
			RequestID: in.RequestID, Status: "failed", Error: pgText(err.Error())})
		return uuid.Nil, uuid.Nil, "failed", err
	}
	if resumed == nil {
		// Checkpoint the committed ids NOW — a crash before FinishProvision
		// must let the stale-lease reclaim RESUME from these, not re-run
		// the fresh tx into a slug conflict + orphan tenant.
		if err := s.q.CheckpointProvision(ctx, db.CheckpointProvisionParams{
			RequestID: in.RequestID, TenantID: toPg(tenantID), OwnerUserID: toPg(userID)}); err != nil {
			_, _ = s.q.FinishProvision(ctx, db.FinishProvisionParams{
				RequestID: in.RequestID, Status: "failed",
				TenantID: toPg(tenantID), OwnerUserID: toPg(userID), Error: pgText(err.Error())})
			return tenantID, uuid.Nil, "failed", err
		}
	}

	if resumed != nil && resumed.OrganizationID.Valid {
		orgID = fromPg(resumed.OrganizationID) // org committed last run too
	} else if s.boot != nil {
		if resumed != nil {
			// Adopt the org a crashed run committed before checkpointing
			// (crash between boot commit and CheckpointProvisionOrg) —
			// (tenant_id, slug) is unique so this is never a foreign org.
			if id, e := s.q.GetOrgIDByTenantSlug(ctx, db.GetOrgIDByTenantSlugParams{
				TenantID: toPg(tenantID), Slug: in.OrgSlug}); e == nil {
				orgID = fromPg(id)
			}
		}
		if orgID == uuid.Nil {
			orgID, err = s.boot(ctx, userID, in.OrgName, in.OrgSlug, in.Template,
				in.OrgType, in.Country, in.City, in.BillingEmail, tenantID)
			if err != nil {
				_, _ = s.q.FinishProvision(ctx, db.FinishProvisionParams{
					RequestID: in.RequestID, Status: "failed",
					TenantID: toPg(tenantID), OwnerUserID: toPg(userID), Error: pgText(err.Error())})
				return tenantID, uuid.Nil, "failed", err
			}
			// Checkpoint the org id NOW — the bootstrap commits outside the
			// provision tx, so a crash before FinishProvision would otherwise
			// leave org_id NULL and a reclaim would re-run CreateOrg into a
			// permanent slug-conflict loop.
			if err := s.q.CheckpointProvisionOrg(ctx, db.CheckpointProvisionOrgParams{
				RequestID: in.RequestID, OrganizationID: toPg(orgID)}); err != nil {
				_, _ = s.q.FinishProvision(ctx, db.FinishProvisionParams{
					RequestID: in.RequestID, Status: "failed",
					TenantID: toPg(tenantID), OrganizationID: toPg(orgID),
					OwnerUserID: toPg(userID), Error: pgText(err.Error())})
				return tenantID, orgID, "failed", err
			}
		}
	}
	err = s.withTx(ctx, func(q *db.Queries) error {
		if orgID != uuid.Nil {
			// §15 backref: the self-serve path sets owner_org_id inside
			// CreateOrg's provision branch; this path must do it here.
			if err := q.SetTenantOwnerOrg(ctx, db.SetTenantOwnerOrgParams{
				ID: toPg(tenantID), OwnerOrgID: toPg(orgID)}); err != nil {
				return err
			}
		}
		if _, err := q.FinishProvision(ctx, db.FinishProvisionParams{
			RequestID: in.RequestID, Status: "done", TenantID: toPg(tenantID),
			OrganizationID: toPg(orgID), OwnerUserID: toPg(userID)}); err != nil {
			return err
		}
		// §15 ordering: org + subscription exist before TenantProvisioned.
		// plan_slug is the provisioned plan (starter trial), not the state.
		return events.Emit(ctx, q, tenantID, orgID, "TenantProvisioned",
			"tenant", tenantID, actor, true,
			map[string]any{"tenant_id": tenantID, "organization_id": orgID,
				"owner_user_id": userID, "plan_slug": "starter"})
	})
	if err != nil {
		_, _ = s.q.FinishProvision(ctx, db.FinishProvisionParams{
			RequestID: in.RequestID, Status: "failed",
			TenantID: toPg(tenantID), OrganizationID: toPg(orgID),
			OwnerUserID: toPg(userID), Error: pgText(err.Error())})
		return tenantID, orgID, "failed", err
	}
	// §15 tail: welcome email (fire-and-forget — never fails the pipeline)
	if s.welcomeFn != nil {
		_ = s.welcomeFn(ctx, in.OwnerEmail, in.OwnerName, userID)
	}
	return tenantID, orgID, "done", nil
}

// StaleProvisions feeds the tick redrive — rows stuck 'processing' past
// the claim lease (worker died mid-pipeline). The redrive re-enqueues
// tenant:provision from the stored payload.
func (s *Service) StaleProvisions(ctx context.Context) ([]db.TenantProvisions, error) {
	return s.q.StaleProvisions(ctx)
}

// ---------- tenants ----------

// ProvisionStatus is the operator's view of a tenant:provision run —
// the 202 from POST /platform/tenants becomes observable (done/failed/
// processing + the created ids).
func (s *Service) ProvisionStatus(ctx context.Context, requestID string) (*db.TenantProvisions, error) {
	row, err := s.q.GetProvision(ctx, requestID)
	if err != nil {
		return nil, ErrNotFound
	}
	return &row, nil
}

func (s *Service) ListTenants(ctx context.Context, status string) ([]db.ListTenantsRow, error) {
	return s.q.ListTenants(ctx, status)
}

func (s *Service) GetTenant(ctx context.Context, id uuid.UUID) (*db.Tenants, error) {
	t, err := s.q.GetTenantRow(ctx, toPg(id))
	if err != nil {
		return nil, ErrNotFound
	}
	return &t, nil
}

// Suspend — §15: suspended tenants block login for non-platform users
// (auth layer enforces), data preserved. Emits TenantSuspended (catalogued).
func (s *Service) SuspendTenant(ctx context.Context, tenantID uuid.UUID, reason string, actor uuid.UUID) error {
	if reason == "" {
		reason = "operator_suspend"
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		cur, err := q.GetTenantRow(ctx, toPg(tenantID))
		if err != nil {
			return ErrNotFound
		}
		if cur.Status == "suspended" {
			return nil // idempotent — a repeat suspend emits no duplicate event/audit
		}
		if _, err := q.SetTenantStatus(ctx, db.SetTenantStatusParams{ID: toPg(tenantID), Status: "suspended"}); err != nil {
			return err
		}
		if err := s.audit(ctx, q, tenantID, actor, "tenant.suspend", "tenant", tenantID,
			map[string]any{"reason": reason}); err != nil {
			return err // Rule 10: failed audit aborts
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "TenantSuspended",
			"tenant", tenantID, actor, true,
			map[string]any{"tenant_id": tenantID, "reason": reason})
	})
}

// Reinstate reverses a suspension back to active.
func (s *Service) ReinstateTenant(ctx context.Context, tenantID uuid.UUID, actor uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		cur, err := q.GetTenantRow(ctx, toPg(tenantID))
		if err != nil {
			return ErrNotFound
		}
		if cur.Status == "active" {
			return nil // idempotent — nothing to reinstate
		}
		if _, err := q.SetTenantStatus(ctx, db.SetTenantStatusParams{ID: toPg(tenantID), Status: "active"}); err != nil {
			return err
		}
		if err := s.audit(ctx, q, tenantID, actor, "tenant.reinstate", "tenant", tenantID, nil); err != nil {
			return err
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "TenantReinstated",
			"tenant", tenantID, actor, true, map[string]any{"tenant_id": tenantID})
	})
}

// ---------- subscriptions / plans ----------

func (s *Service) ListSubscriptions(ctx context.Context, state string) ([]db.ListSubscriptionsRow, error) {
	return s.q.ListSubscriptions(ctx, state)
}

// OverrideSubscription is the audited operator path: plan/state changes
// emit SubscriptionChanged so the entitlement cache flushes (Rule 25).
var subscriptionStates = map[string]bool{
	"trial": true, "active": true, "past_due": true,
	"grace": true, "restricted": true, "cancelled": true,
}

func (s *Service) OverrideSubscription(ctx context.Context, tenantID, subID uuid.UUID, plan, state string, graceEnd *time.Time, actor uuid.UUID) error {
	if plan == "" && state == "" {
		return ErrBadValue
	}
	if state != "" && !subscriptionStates[state] {
		return ErrBadValue
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		prev, err := q.GetSubscription(ctx, toPg(tenantID))
		if err != nil {
			return ErrNotFound
		}
		var ge pgtype.Timestamptz
		if graceEnd != nil {
			ge = pgtype.Timestamptz{Time: *graceEnd, Valid: true}
		}
		newPlan := prev.PlanSlug
		if plan != "" {
			newPlan = plan
		}
		newState := prev.State
		if state != "" {
			newState = state
		}
		n, err := q.OverrideSubscription(ctx, db.OverrideSubscriptionParams{
			ID: toPg(subID), TenantID: toPg(tenantID), PlanSlug: newPlan,
			State: newState, GraceEndsAt: ge})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if err := s.audit(ctx, q, tenantID, actor, "subscription.override", "subscription", subID,
			map[string]any{"from_plan": prev.PlanSlug, "to_plan": newPlan, "to_state": state}); err != nil {
			return err
		}
		return events.Emit(ctx, q, tenantID, uuid.Nil, "SubscriptionChanged",
			"subscription", subID, actor, true,
			map[string]any{"tenant_id": tenantID, "from": prev.PlanSlug,
				"to": newPlan, "plan_slug": newPlan})
	})
}

func (s *Service) UpsertPlan(ctx context.Context, p db.UpsertPlanParams, actor uuid.UUID) (*db.Plans, error) {
	var row db.Plans
	err := s.withTx(ctx, func(q *db.Queries) error {
		r, err := q.UpsertPlan(ctx, p)
		if err != nil {
			return err
		}
		row = r
		return s.audit(ctx, q, uuid.Nil, actor, "plan.upsert", "plan", uuid.Nil,
			map[string]any{"slug": p.Slug})
	})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ---------- deployments ----------

func (s *Service) ListDeployments(ctx context.Context, tenantID uuid.UUID) ([]db.ListDeploymentsRow, error) {
	return s.q.ListDeployments(ctx, toPg(tenantID))
}

func (s *Service) CreateDeployment(ctx context.Context, tenantID uuid.UUID, kind, edition, key string, actor uuid.UUID) (*db.Deployments, error) {
	if kind == "" {
		kind = "self_hosted"
	}
	if edition == "" {
		edition = "community"
	}
	var row db.Deployments
	err := s.withTx(ctx, func(q *db.Queries) error {
		if _, err := q.GetTenantRow(ctx, toPg(tenantID)); err != nil {
			return ErrNotFound
		}
		r, err := q.CreateDeployment(ctx, db.CreateDeploymentParams{
			TenantID: toPg(tenantID), Kind: kind, Edition: edition, DeploymentKey: key})
		if err != nil {
			return err
		}
		row = r
		if err := events.Emit(ctx, q, tenantID, uuid.Nil, "DeploymentRegistered",
			"deployment", fromPg(r.ID), actor, true,
			map[string]any{"deployment_id": fromPg(r.ID), "tenant_id": tenantID,
				"kind": kind, "edition": edition}); err != nil {
			return err
		}
		return s.audit(ctx, q, tenantID, actor, "deployment.create", "deployment", fromPg(r.ID), nil)
	})
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// ---------- support impersonation (§15, Rule 10) ----------

const impersonationTTL = 30 * time.Minute

// Impersonate — audit row commits BEFORE the session row (Rule 10: a
// failed audit write aborts). TTL is fixed at 30min per §15; the returned
// session id is what the token layer embeds for the banner.
func (s *Service) Impersonate(ctx context.Context, operatorID, tenantID uuid.UUID, targetEmail, reason, ip, ua string) (uuid.UUID, string, time.Time, error) {
	if strings.TrimSpace(reason) == "" {
		return uuid.Nil, "", time.Time{}, ErrBadValue // support access needs a recorded reason (Rule 10)
	}
	if _, err := s.q.GetTenantRow(ctx, toPg(tenantID)); err != nil {
		return uuid.Nil, "", time.Time{}, ErrNotFound
	}
	u, err := s.q.GetUserByEmail(ctx, targetEmail)
	if err != nil || u.Status != "active" {
		return uuid.Nil, "", time.Time{}, ErrNotFound
	}
	// the target must actually belong to the tenant — request-supplied
	// tenant_id alone is not proof (Rule 24).
	ok, err := s.q.UserInTenant(ctx, db.UserInTenantParams{
		UserID: u.ID, TenantID: toPg(tenantID)})
	if err != nil || !ok {
		return uuid.Nil, "", time.Time{}, ErrNotFound
	}
	// a target holding ANY platform role yields platform access to the
	// impersonating operator — support access is scoped to tenant users
	// (§09: never a universal bypass). Refuse, don't mint privilege.
	// Fail CLOSED: a check error denies rather than mints unchecked power.
	plat, err := s.q.UserHasAnyPlatformRole(ctx, u.ID)
	if err != nil {
		return uuid.Nil, "", time.Time{}, fmt.Errorf("platform_role_check: %w", err)
	}
	if plat {
		return uuid.Nil, "", time.Time{}, ErrImpersonationDenied
	}
	expires := time.Now().Add(impersonationTTL)
	var sid uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		if err := s.audit(ctx, q, tenantID, operatorID, "impersonation.start",
			"user", fromPg(u.ID),
			map[string]any{"target_email": targetEmail, "reason": reason}); err != nil {
			return err
		}
		ss, err := q.CreateSupportSession(ctx, db.CreateSupportSessionParams{
			OperatorUserID: toPg(operatorID), TargetUserID: u.ID,
			TenantID: toPg(tenantID), Reason: pgText(reason),
			ExpiresAt: pgtype.Timestamptz{Time: expires, Valid: true}})
		if err != nil {
			return err
		}
		sid = fromPg(ss.ID)
		return events.Emit(ctx, q, tenantID, uuid.Nil, "SupportSessionStarted",
			"support_session", sid, operatorID, true,
			map[string]any{"session_id": sid, "tenant_id": tenantID,
				"agent_id": operatorID})
	})
	if err != nil {
		return uuid.Nil, "", time.Time{}, err
	}
	// audit+session row committed — mint the operator's support token.
	if s.sessionFn == nil {
		return uuid.Nil, "", time.Time{}, errors.New("session minter not wired")
	}
	token, _, err := s.sessionFn(ctx, sid, fromPg(u.ID), impersonationTTL, ip, ua)
	if err != nil {
		// don't leave a live support_sessions row for a token that was
		// never minted — the banner would show a phantom session.
		_, _ = s.q.EndSupportSession(ctx, toPg(sid))
		return uuid.Nil, "", time.Time{}, err
	}
	return sid, token, expires, nil
}

func (s *Service) EndImpersonation(ctx context.Context, sid, actor uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		// fetch the session's tenant for the event row before ending it.
		ss, err := q.GetSupportSession(ctx, toPg(sid))
		if err != nil {
			return ErrSessionExpired
		}
		n, err := q.EndSupportSession(ctx, toPg(sid))
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrSessionExpired
		}
		// explicit end must cut access NOW — revoke the minted session row
		// rather than waiting out its TTL.
		if _, err := q.RevokeSessionsForSupport(ctx, toPg(sid)); err != nil {
			return err
		}
		if err := events.Emit(ctx, q, fromPg(ss.TenantID), uuid.Nil, "SupportSessionEnded",
			"support_session", sid, actor, true,
			map[string]any{"session_id": sid}); err != nil {
			return err
		}
		return s.audit(ctx, q, fromPg(ss.TenantID), actor, "impersonation.end", "support_session", sid, nil)
	})
}

// ---------- flags / audit / usage ----------

func (s *Service) ListFlags(ctx context.Context) ([]db.FeatureFlags, error) {
	return s.q.ListFeatureFlags(ctx)
}

func (s *Service) SetFlag(ctx context.Context, key string, enabled bool, rollout json.RawMessage, actor uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		if err := q.UpsertFeatureFlag(ctx, db.UpsertFeatureFlagParams{
			Key: key, Enabled: enabled, Rollout: rollout}); err != nil {
			return err
		}
		return s.audit(ctx, q, uuid.Nil, actor, "flag.set", "feature_flag", uuid.Nil,
			map[string]any{"key": key, "enabled": enabled})
	})
}

func (s *Service) AuditEvents(ctx context.Context, tenantID uuid.UUID, action string) ([]db.AuditEvents, error) {
	return s.q.ListAuditEvents(ctx, db.ListAuditEventsParams{
		TenantID: toPg(tenantID), Action: action})
}

func (s *Service) Usage(ctx context.Context, tenantID uuid.UUID) ([]db.UsageMeters, error) {
	return s.q.ListUsage(ctx, toPg(tenantID))
}

// audit writes the append-only row (Rule 10) — callers run it inside their
// own tx so a failure rolls back the audited operation too.
func (s *Service) audit(ctx context.Context, q *db.Queries, tenantID, actor uuid.UUID, action, targetType string, targetID uuid.UUID, payload map[string]any) error {
	var ctxb []byte
	if payload != nil {
		ctxb, _ = json.Marshal(payload)
	}
	return q.InsertAuditEvent(ctx, db.InsertAuditEventParams{
		TenantID: toPg(tenantID), ActorID: toPg(actor), Action: action,
		TargetType: pgText(targetType), TargetID: toPg(targetID),
		Context: ctxb})
}

// LiveSupportSessions feeds the §15 banner — sessions targeting the caller.
func (s *Service) LiveSupportSessions(ctx context.Context, userID uuid.UUID) ([]db.LiveSupportSessionsForUserRow, error) {
	return s.q.LiveSupportSessionsForUser(ctx, toPg(userID))
}

// LiveSupportSessionsForOrg — the org-admin side of the banner: live
// impersonation sessions targeting ANY member of the org, not just the
// caller (org.admin.settings surface).
func (s *Service) LiveSupportSessionsForOrg(ctx context.Context, orgID uuid.UUID) ([]db.LiveSupportSessionsForOrgRow, error) {
	return s.q.LiveSupportSessionsForOrg(ctx, toPg(orgID))
}

// WelcomeMail sends the provisioned owner's credential link — wired in
// main (auth mints the action token, mailer sends). Fire-and-forget: mail
// failure never fails the provision (Rule 8 fan-out semantics).
type WelcomeMail func(ctx context.Context, email, displayName string, userID uuid.UUID) error

func (s *Service) SetWelcomeMail(fn WelcomeMail) { s.welcomeFn = fn }

func (s *Service) SetSessionMinter(fn func(ctx context.Context, supportSessionID, targetUserID uuid.UUID, ttl time.Duration, ip, ua string) (string, uuid.UUID, error)) {
	s.sessionFn = fn
}

// ---------- tenant export (Rule 28 — §13.06) ----------

// validIdent guards the dynamic table/column names that come back from
// information_schema — DB-sourced but never trusted raw into SQL text.
var validIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// ExportTenant is the synchronous GET path — audits, then builds the doc.
func (s *Service) ExportTenant(ctx context.Context, tenantID, actor uuid.UUID) ([]byte, error) {
	if err := s.auditExport(ctx, tenantID, actor); err != nil {
		return nil, err // Rule 10: failed audit write aborts
	}
	return s.buildExport(ctx, tenantID)
}

func (s *Service) auditExport(ctx context.Context, tenantID, actor uuid.UUID) error {
	return s.withTx(ctx, func(q *db.Queries) error {
		return s.audit(ctx, q, tenantID, actor, "tenant.export", "tenant", tenantID, nil)
	})
}

// buildExport produces the full Rule-28 data export: every org-scoped and
// tenant-scoped table dumped to JSON, plus a credential-free user
// projection (ADR-020 data minimization).
func (s *Service) buildExport(ctx context.Context, tenantID uuid.UUID) ([]byte, error) {
	if _, err := s.q.GetTenantRow(ctx, toPg(tenantID)); err != nil {
		return nil, ErrNotFound
	}
	orgIDs, err := s.q.ExportTenantOrgIDs(ctx, toPg(tenantID))
	if err != nil {
		return nil, err
	}
	orgArr := make([]pgtype.UUID, len(orgIDs))
	for i, id := range orgIDs {
		orgArr[i] = id
	}
	orgScoped, err := s.q.ExportScopedTables(ctx, "organization_id")
	if err != nil {
		return nil, err
	}
	tnScoped, err := s.q.ExportScopedTables(ctx, "tenant_id")
	if err != nil {
		return nil, err
	}
	dump := func(table, col string, arg any) (json.RawMessage, error) {
		if !validIdent.MatchString(table) || !validIdent.MatchString(col) {
			return nil, nil
		}
		var b []byte
		err := s.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT COALESCE(jsonb_agg(t),'[]'::jsonb) FROM %s t WHERE t.%s = ANY($1)`,
			`"`+table+`"`, col), arg).Scan(&b)
		if err != nil {
			return nil, fmt.Errorf("export %s: %w", table, err)
		}
		return b, nil
	}
	orgTables := map[string]json.RawMessage{}
	for _, tbl := range orgScoped {
		rows, err := dump(tbl, "organization_id", orgArr)
		if err != nil {
			return nil, err
		}
		if rows != nil {
			orgTables[tbl] = rows
		}
	}
	tnTables := map[string]json.RawMessage{}
	tnArr := []pgtype.UUID{toPg(tenantID)}
	for _, tbl := range tnScoped {
		rows, err := dump(tbl, "tenant_id", tnArr)
		if err != nil {
			return nil, err
		}
		if rows != nil {
			tnTables[tbl] = rows
		}
	}
	// children keyed by FK to a scoped parent — they carry no scope column
	// themselves, so they're joined through the parent's scope instead.
	// Two passes: org-scoped parents ($1; chat_messages.channel_id,
	// minutes.activity_id, document_versions.document_id, ...) and
	// tenant-scoped parents ($2; user_role_assignments.role_id →
	// user_roles.tenant_id, children of organizations.id, ...). FKs to
	// different parents OR together (an edge kept by either side).
	conds := map[string][]string{}
	orgChild := map[string]bool{}
	for _, pass := range []struct {
		col  string
		cond string // %s = child col, parent col, parent table
		org  bool
	}{
		{"organization_id", "t.%s IN (SELECT %s FROM %s WHERE organization_id = ANY($1))", true},
		{"tenant_id", "t.%s IN (SELECT %s FROM %s WHERE tenant_id = ANY($2))", false},
	} {
		fks, err := s.q.ExportChildFKs(ctx, pass.col)
		if err != nil {
			return nil, err
		}
		for _, fk := range fks {
			if !validIdent.MatchString(fk.ChildTable) || !validIdent.MatchString(fk.ChildCol) ||
				!validIdent.MatchString(fk.ParentTable) || !validIdent.MatchString(fk.ParentCol) {
				continue
			}
			conds[fk.ChildTable] = append(conds[fk.ChildTable], fmt.Sprintf(pass.cond,
				fk.ChildCol, `"`+fk.ParentCol+`"`, `"`+fk.ParentTable+`"`))
			if pass.org {
				orgChild[fk.ChildTable] = true
			}
		}
	}
	for tbl, cs := range conds {
		_, inOrg := orgTables[tbl]
		_, inTn := tnTables[tbl]
		if inOrg || inTn || len(cs) == 0 {
			continue // directly scoped — already dumped
		}
		var b []byte
		if err := s.pool.QueryRow(ctx, fmt.Sprintf(
			`SELECT COALESCE(jsonb_agg(t),'[]'::jsonb) FROM %s t WHERE %s`,
			`"`+tbl+`"`, strings.Join(cs, " OR ")), orgArr, tnArr).Scan(&b); err != nil {
			return nil, fmt.Errorf("export %s: %w", tbl, err)
		}
		if orgChild[tbl] {
			orgTables[tbl] = b
		} else {
			tnTables[tbl] = b
		}
	}
	users, err := s.q.ExportTenantUsers(ctx, toPg(tenantID))
	if err != nil {
		return nil, err
	}
	prefs, err := s.q.ExportOrgNotificationPrefs(ctx, orgArr)
	if err != nil {
		return nil, err
	}
	userNotifs, err := s.q.ExportTenantUserNotifications(ctx, orgArr)
	if err != nil {
		return nil, err
	}
	tnRow, err := s.q.ExportTenantRow(ctx, toPg(tenantID))
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"tenant_id":                tenantID,
		"exported_at":              time.Now().UTC(),
		"tenant":                   json.RawMessage(tnRow),
		"tenant_tables":            tnTables,
		"org_tables":               orgTables,
		"users":                    json.RawMessage(users),
		"notification_preferences": json.RawMessage(prefs),
		"user_notifications":       json.RawMessage(userNotifs),
	})
}

// ---------- async tenant export (tenant:export worker — §13.06) ----------

// exportStaleAfter is the worker lease for a 'processing' export — past it,
// DownloadTenantExport marks the row failed instead of 202-ing forever.
const exportStaleAfter = 15 * time.Minute

// RequestTenantExport audits + creates the durable row; the handler
// enqueues tenant:export {export_id} (Rule 8 — the request never blocks
// on the dump).
func (s *Service) RequestTenantExport(ctx context.Context, tenantID, actor uuid.UUID) (uuid.UUID, error) {
	if _, err := s.q.GetTenantRow(ctx, toPg(tenantID)); err != nil {
		return uuid.Nil, ErrNotFound
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		// audit-first (Rule 10) — the row + audit commit together.
		if err := s.audit(ctx, q, tenantID, actor, "tenant.export", "tenant", tenantID,
			map[string]any{"mode": "async"}); err != nil {
			return err
		}
		row, err := q.CreateTenantExport(ctx, db.CreateTenantExportParams{
			TenantID: toPg(tenantID), RequestedBy: toPg(actor)})
		if err != nil {
			return err
		}
		id = fromPg(row.ID)
		return nil
	})
	return id, err
}

// RunTenantExport is the tenant:export worker body: build → store blob →
// mark done. Idempotent (Rule 27): a retry re-builds and the done CAS
// no-ops; a failure marks the row failed with the error.
func (s *Service) RunTenantExport(ctx context.Context, exportID uuid.UUID) error {
	e, err := s.q.GetTenantExportByID(ctx, toPg(exportID))
	if err != nil {
		return nil // row gone
	}
	if e.Status == "done" {
		return nil // replay after success
	}
	if s.store == nil {
		return errors.New("export store not wired")
	}
	fail := func(err error) error {
		_ = s.q.MarkTenantExportFailed(ctx, db.MarkTenantExportFailedParams{
			ID: e.ID, Error: pgText(err.Error())})
		return err
	}
	data, err := s.buildExport(ctx, fromPg(e.TenantID))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			_ = fail(err)
			return nil // tenant gone — permanent, not worth retrying
		}
		return fail(err)
	}
	key := "tenant-export/" + exportID.String() + ".json"
	if err := s.store.Put(ctx, key, bytes.NewReader(data)); err != nil {
		return fail(err)
	}
	n, err := s.q.MarkTenantExportDone(ctx, db.MarkTenantExportDoneParams{
		ID: e.ID, StorageKey: pgText(key)})
	if err != nil {
		return err
	}
	if n == 0 && e.Status == "processing" {
		return errors.New("export row lost processing claim") // raced — retry
	}
	return nil
}

// DownloadTenantExport streams a finished export blob. Returns
// (reader, status, error): status lets the handler 202 a still-running job.
func (s *Service) DownloadTenantExport(ctx context.Context, tenantID, exportID uuid.UUID) (io.ReadCloser, string, error) {
	e, err := s.q.GetTenantExport(ctx, db.GetTenantExportParams{ID: toPg(exportID), TenantID: toPg(tenantID)})
	if err != nil {
		return nil, "", ErrNotFound
	}
	// self-heal: a row stuck 'processing' past the worker lease means the
	// enqueue failed after commit — mark it failed so the client sees a
	// terminal state, not an eternal 202.
	if e.Status == "processing" && time.Since(e.CreatedAt.Time) > exportStaleAfter {
		_ = s.q.MarkTenantExportFailed(ctx, db.MarkTenantExportFailedParams{
			ID: e.ID, Error: pgText("export never completed — worker never ran")})
		return nil, "failed", nil
	}
	if e.Status != "done" {
		return nil, e.Status, nil
	}
	r, err := s.store.Open(ctx, e.StorageKey.String)
	if err != nil {
		return nil, "", err
	}
	return r, e.Status, nil
}
