// Ref: oga9-engineering/sections/20-projects.md — proposal → passed-
// resolution approval → milestones → updates → donors → close with a
// completion report; edges land in entity_links (ADR-015).
package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound     = errors.New("not found")
	ErrBadValue     = errors.New("bad value")
	ErrBadState     = errors.New("bad_project_state")
	ErrNoResolution = errors.New("resolution_required") // activate needs a passed resolution
)

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }
func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: !t.IsZero()} }
func nInt32(v int32) pgtype.Int4     { return pgtype.Int4{Int32: v, Valid: v != 0} }
func nInt(v int64) pgtype.Int8       { return pgtype.Int8{Int64: v, Valid: v != 0} }

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// archiveFn lands the completion report in documents (Rule 26 seam).
	archiveFn func(ctx context.Context, oc *tenant.OrgCtx, title string, r io.Reader) (uuid.UUID, error)
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

func (s *Service) SetArchive(fn func(context.Context, *tenant.OrgCtx, string, io.Reader) (uuid.UUID, error)) {
	s.archiveFn = fn
}

func (s *Service) withTx(ctx context.Context, fn func(*db.Queries) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(s.q.WithTx(tx)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) settingInt(ctx context.Context, orgID uuid.UUID, key string) int64 {
	raw, err := s.q.GetOrgSetting(ctx, db.GetOrgSettingParams{OrganizationID: toPg(orgID), Key: key})
	if err != nil {
		return 0
	}
	var v float64
	if json.Unmarshal(raw, &v) == nil {
		return int64(v)
	}
	return 0
}

// ---------- lifecycle ----------

// Propose — a proposal enters 'proposed'; approval needs a passed
// resolution (§20 rule — never a permission check alone).
func (s *Service) Propose(ctx context.Context, oc *tenant.OrgCtx, title, description string,
	unitID uuid.UUID, startsOn, targetOn time.Time) (uuid.UUID, error) {
	if title == "" {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		pid, err := q.CreateProject(ctx, db.CreateProjectParams{
			OrgID: toPg(oc.OrgID), Title: title, Description: pgText(description),
			UnitID: toPg(unitID), StartsOn: pgDate(startsOn), TargetOn: pgDate(targetOn)})
		if err != nil {
			return err
		}
		id = fromPg(pid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ProjectProposed",
			"project", id, oc.UserID, true,
			map[string]any{"project_id": id, "title": title})
	})
	return id, err
}

func (s *Service) List(ctx context.Context, oc *tenant.OrgCtx, state string) ([]db.Projects, error) {
	return s.q.ListProjects(ctx, db.ListProjectsParams{OrgID: toPg(oc.OrgID), State: pgText(state)})
}

// Get — the project plus its cadence flag: updates older than
// org.projects.update_interval_days are stale (evidence cadence rule).
func (s *Service) Get(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) (map[string]any, error) {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, ErrNotFound
	}
	out := map[string]any{"project": p, "updates_stale": false}
	if days := s.settingInt(ctx, oc.OrgID, "org.projects.update_interval_days"); days > 0 {
		if last, err := s.q.LatestUpdate(ctx, p.ID); err != nil ||
			time.Since(last.Time) > time.Duration(days)*24*time.Hour {
			out["updates_stale"] = true
		}
	}
	return out, nil
}

// Approve — §20: only a PASSED resolution can approve a proposal; the
// resolution_id edge lands on the project + entity_links (approved_by).
func (s *Service) Approve(ctx context.Context, oc *tenant.OrgCtx, pid, resolutionID uuid.UUID) error {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	outcome, err := s.q.ResolutionOutcome(ctx, db.ResolutionOutcomeParams{
		ID: toPg(resolutionID), OrganizationID: toPg(oc.OrgID)})
	if err != nil || (outcome != "passed" && outcome != "amended_passed") {
		return ErrNoResolution
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.ApproveProject(ctx, db.ApproveProjectParams{ID: p.ID, ResolutionID: toPg(resolutionID)})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState // not 'proposed'
		}
		// approval edge — entity_links consumer also derives it on replay
		// (edge UNIQUE → ON CONFLICT DO NOTHING makes both paths safe).
		if err := q.LinkEntitiesForProject(ctx, db.LinkEntitiesForProjectParams{
			OrgID: toPg(oc.OrgID), SourceType: "project", SourceID: p.ID,
			TargetType: "resolution", TargetID: toPg(resolutionID), Rel: "approved_by",
			CreatedBy: toPg(oc.UserID)}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ProjectApproved",
			"project", pid, oc.UserID, true,
			map[string]any{"project_id": pid, "resolution_id": resolutionID})
	})
}

// Transition — active/suspended/abandoned moves. 'active' requires the
// prior approval edge (test 1: activate without resolution → blocked).
func (s *Service) Transition(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID, action string) error {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	var to, from, ev string
	switch action {
	case "start":
		to, from, ev = "active", "approved", "ProjectStarted"
		if !p.ResolutionID.Valid {
			return ErrNoResolution
		}
	case "resume":
		to, from, ev = "active", "suspended", "ProjectStarted"
	case "suspend":
		to, from, ev = "suspended", "active", "ProjectSuspended"
	case "abandon":
		to, ev = "abandoned", "ProjectSuspended"
		if p.State != "proposed" && p.State != "approved" && p.State != "suspended" {
			return ErrBadState
		}
		from = p.State
	default:
		return ErrBadValue
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetProjectState(ctx, db.SetProjectStateParams{ID: p.ID, State: to, State_2: from})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev,
			"project", pid, oc.UserID, action != "abandon",
			map[string]any{"project_id": pid, "org_id": oc.OrgID})
	})
}

// ---------- milestones / updates ----------

func (s *Service) AddMilestone(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID,
	title string, weight int32, dueOn time.Time) (uuid.UUID, error) {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	if title == "" || weight < 1 {
		return uuid.Nil, ErrBadValue
	}
	if p.State == "completed" || p.State == "abandoned" {
		return uuid.Nil, ErrBadState
	}
	id, err := s.q.AddMilestone(ctx, db.AddMilestoneParams{
		ProjectID: p.ID, Title: title, Weight: weight, DueOn: pgDate(dueOn)})
	return fromPg(id), err
}

func (s *Service) ListMilestones(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) ([]db.ProjectMilestones, error) {
	if _, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return nil, ErrNotFound
	}
	return s.q.ListMilestones(ctx, toPg(pid))
}

// CompleteMilestone — weighted progress recomputes (done/total weights)
// and lands on the project (test 2).
func (s *Service) CompleteMilestone(ctx context.Context, oc *tenant.OrgCtx, pid, mid uuid.UUID) error {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.CompleteMilestone(ctx, db.CompleteMilestoneParams{ID: toPg(mid), ProjectID: p.ID})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound // unknown or already-completed milestone
		}
		pr, err := q.MilestoneProgress(ctx, p.ID)
		if err != nil {
			return err
		}
		pct := int32(0)
		if pr.Total > 0 {
			pct = int32(pr.Done * 100 / pr.Total)
		}
		if err := q.SetProjectProgress(ctx, db.SetProjectProgressParams{ID: p.ID, ProgressPct: pct}); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "MilestoneCompleted",
			"milestone", mid, oc.UserID, true,
			map[string]any{"milestone_id": mid, "project_id": pid, "progress_pct": pct})
	})
}

// PostUpdate — progress evidence; cadence is enforced via the stale flag
// on Get (org.projects.update_interval_days), never by rejecting posts.
func (s *Service) PostUpdate(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID,
	body string, progressPct int32) (uuid.UUID, error) {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	if body == "" {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		uid, err := q.AddUpdate(ctx, db.AddUpdateParams{
			ProjectID: p.ID, AuthorID: toPg(oc.MembershipID), Body: body,
			ProgressPct: nInt32(progressPct)})
		if err != nil {
			return err
		}
		id = fromPg(uid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ProjectUpdatePublished",
			"project_update", id, oc.UserID, false,
			map[string]any{"update_id": id, "project_id": pid})
	})
	return id, err
}

func (s *Service) ListUpdates(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) ([]db.ProjectUpdates, error) {
	if _, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return nil, ErrNotFound
	}
	return s.q.ListUpdates(ctx, toPg(pid))
}

// ---------- vendors / donors ----------

func (s *Service) AddVendor(ctx context.Context, oc *tenant.OrgCtx, name, kind string, contact json.RawMessage) (uuid.UUID, error) {
	if name == "" {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		vid, err := q.CreateVendor(ctx, db.CreateVendorParams{
			OrgID: toPg(oc.OrgID), Name: name, Contact: contact, Kind: pgText(kind)})
		if err != nil {
			return err
		}
		id = fromPg(vid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "VendorEngaged",
			"vendor", id, oc.UserID, false,
			map[string]any{"vendor_id": id, "name": name})
	})
	return id, err
}

func (s *Service) ListVendors(ctx context.Context, oc *tenant.OrgCtx) ([]db.Vendors, error) {
	return s.q.ListVendors(ctx, toPg(oc.OrgID))
}

// RecordDonor — the payment lives in finance; the donor row carries the
// linkage (payment_id) so money never moves off-ledger (§20/Rule 19).
func (s *Service) RecordDonor(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID,
	donorName string, membershipID uuid.UUID, amountMinor int64, currency string,
	paymentID uuid.UUID) (uuid.UUID, error) {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return uuid.Nil, ErrNotFound
	}
	if donorName == "" {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		did, err := q.AddDonor(ctx, db.AddDonorParams{
			ProjectID: p.ID, DonorName: donorName, MembershipID: toPg(membershipID),
			AmountMinor: nInt(amountMinor), Currency: pgText(currency), PaymentID: toPg(paymentID)})
		if err != nil {
			return err
		}
		id = fromPg(did)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "DonorRecorded",
			"project_donor", id, oc.UserID, false,
			map[string]any{"donor_id": id, "project_id": pid, "amount_minor": amountMinor})
	})
	return id, err
}

func (s *Service) ListDonors(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) ([]db.ProjectDonors, error) {
	if _, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return nil, ErrNotFound
	}
	return s.q.ListDonors(ctx, toPg(pid))
}

// ---------- close + graph ----------

// Close — completion report auto-generated → documents artifact +
// entity_links edge + ProjectClosed (test 4).
func (s *Service) Close(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) error {
	p, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	if p.State == "completed" {
		return nil // idempotent
	}
	// build the completion report before the state flips (needs read anyway)
	milestones, _ := s.q.ListMilestones(ctx, p.ID)
	donors, _ := s.q.ListDonors(ctx, p.ID)
	report := map[string]any{
		"project_id": pid, "title": p.Title, "outcome": "completed",
		"progress_pct": p.ProgressPct, "milestones": milestones, "donors": donors,
		"generated_at": time.Now().UTC(),
	}
	var docID uuid.UUID
	if s.archiveFn != nil {
		raw, _ := json.MarshalIndent(report, "", "  ")
		id, err := s.archiveFn(ctx, oc, "project-completion-"+pid.String()[:8], bytes.NewReader(raw))
		if err != nil {
			return err
		}
		docID = id
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.CompleteProject(ctx, p.ID)
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState
		}
		if docID != uuid.Nil {
			if err := q.LinkEntitiesForProject(ctx, db.LinkEntitiesForProjectParams{
				OrgID: toPg(oc.OrgID), SourceType: "project", SourceID: toPg(pid),
				TargetType: "document", TargetID: toPg(docID), Rel: "archived_as",
				CreatedBy: toPg(oc.UserID)}); err != nil {
				return err
			}
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "ProjectClosed",
			"project", pid, oc.UserID, true,
			map[string]any{"project_id": pid, "outcome": "completed",
				"document_id": docID})
	})
}

// Graph — the entity_links neighbourhood around the project node.
func (s *Service) Graph(ctx context.Context, oc *tenant.OrgCtx, pid uuid.UUID) ([]db.ProjectGraphRow, error) {
	if _, err := s.q.GetProject(ctx, db.GetProjectParams{ID: toPg(pid), OrganizationID: toPg(oc.OrgID)}); err != nil {
		return nil, ErrNotFound
	}
	return s.q.ProjectGraph(ctx, toPg(pid))
}

// PublicProjects — /directory showcase (no OrgCtx — slug-scoped read).
func (s *Service) PublicProjects(ctx context.Context, slug string) ([]db.PublicOrgProjectsRow, error) {
	if slug == "" {
		return nil, fmt.Errorf("%w", ErrBadValue)
	}
	return s.q.PublicOrgProjects(ctx, slug)
}
