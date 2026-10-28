// Ref: oga9-engineering/sections/19-analytics.md — digital-twin read side:
// dashboards serve async snapshots (no per-request domain scans), health
// scores carry a versioned formula, reports land as document artifacts.
package analytics

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound   = errors.New("not found")
	ErrBadValue   = errors.New("bad value")
	ErrCapability = errors.New("capability_required")
)

const (
	// healthFormulaVersion — §19: formula is versioned + shown to the org.
	healthFormulaVersion = "v1"
	// reportKinds per §19.
	reportMonthly = "monthly"
	reportAnnual  = "annual"
)

var snapshotRoles = []string{"president", "treasurer", "secretary", "member"}
var reportKinds = map[string]bool{"monthly": true, "annual": true, "custom": true, "handover": true}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
	// seams (Rule 26): section producers feed each role's snapshot from the
	// owning modules' services; archiveFn lands report artifacts in
	// documents; capFn gates Business-tier surfaces (capability.consolidation);
	// myFn feeds /me/analytics.
	sections  map[string]func(ctx context.Context, oc *tenant.OrgCtx, role string) (any, error)
	archiveFn func(ctx context.Context, oc *tenant.OrgCtx, title string, r io.Reader) (uuid.UUID, error)
	capFn     func(ctx context.Context, tenantID uuid.UUID, capability string) bool
	myFn      func(ctx context.Context, userID uuid.UUID) (any, error)
	// permFn resolves the viewer's dashboard role from their permission
	// nodes — a ?role= query param must never pick another seat's view
	// (§19: content filtered by the viewer's permissions; Rule 24).
	permFn func(ctx context.Context, userID, orgID uuid.UUID, node string) bool
	// docFn streams a report's stored artifact (Rule 26 — documents owns
	// the blob); branchFn produces the per-branch roll-up blocks.
	docFn    func(ctx context.Context, oc *tenant.OrgCtx, docID uuid.UUID) (io.ReadCloser, string, error)
	branchFn func(ctx context.Context, oc *tenant.OrgCtx) ([]map[string]any, error)
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

func (s *Service) SetSection(name string, fn func(context.Context, *tenant.OrgCtx, string) (any, error)) {
	if s.sections == nil {
		s.sections = map[string]func(context.Context, *tenant.OrgCtx, string) (any, error){}
	}
	s.sections[name] = fn
}

func (s *Service) SetArchive(fn func(context.Context, *tenant.OrgCtx, string, io.Reader) (uuid.UUID, error)) {
	s.archiveFn = fn
}

func (s *Service) SetCapability(fn func(context.Context, uuid.UUID, string) bool) { s.capFn = fn }
func (s *Service) SetMe(fn func(context.Context, uuid.UUID) (any, error))         { s.myFn = fn }
func (s *Service) SetPermissions(fn func(context.Context, uuid.UUID, uuid.UUID, string) bool) {
	s.permFn = fn
}

// SetDocArtifact wires the documents download seam — the run row's
// document_id resolves to a real artifact stream.
func (s *Service) SetDocArtifact(fn func(context.Context, *tenant.OrgCtx, uuid.UUID) (io.ReadCloser, string, error)) {
	s.docFn = fn
}

// SetBranches wires the per-branch roll-up producer (§19 consolidated).
func (s *Service) SetBranches(fn func(context.Context, *tenant.OrgCtx) ([]map[string]any, error)) {
	s.branchFn = fn
}

// DownloadReport — the artifact stream for a run row: resolves the stored
// document through the documents seam; falls back to the run row JSON when
// the document module is unwired (self-host minimal builds).
func (s *Service) DownloadReport(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID) (io.ReadCloser, string, error) {
	run, err := s.q.GetReportRun(ctx, db.GetReportRunParams{ID: toPg(rid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return nil, "", ErrNotFound
	}
	if s.docFn == nil || !run.DocumentID.Valid {
		raw, _ := json.Marshal(run)
		return io.NopCloser(strings.NewReader(string(raw))), "report.json", nil
	}
	return s.docFn(ctx, oc, uuid.UUID(run.DocumentID.Bytes))
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

// ---------- dashboard snapshots ----------

// roleFor resolves the viewer's dashboard role server-side from their
// permission nodes (§19 role-resolved). Highest seat wins.
func (s *Service) roleFor(ctx context.Context, oc *tenant.OrgCtx) string {
	if s.permFn != nil {
		switch {
		case s.permFn(ctx, oc.UserID, oc.OrgID, "org.governance.appointments.manage"):
			return "president"
		case s.permFn(ctx, oc.UserID, oc.OrgID, "org.finance.payments.approve"):
			return "treasurer"
		case s.permFn(ctx, oc.UserID, oc.OrgID, "org.minutes.approve"):
			return "secretary"
		}
	}
	return "member"
}

// Dashboard serves the async snapshot for the viewer's resolved role —
// §19: dashboards never scan domain tables per-request, and the role is
// derived from permissions, never a request parameter. A missing
// snapshot falls back to the 30d event projection (cheap aggregate).
func (s *Service) Dashboard(ctx context.Context, oc *tenant.OrgCtx) (json.RawMessage, error) {
	role := s.roleFor(ctx, oc)
	if row, err := s.q.GetSnapshot(ctx, db.GetSnapshotParams{
		OrganizationID: toPg(oc.OrgID), Role: role}); err == nil {
		return row.Snapshot, nil
	}
	// cold-start: no per-request domain scans (§19) — serve the 30d
	// domain_events projection only; section producers fill in at the
	// next 15min snapshot refresh.
	counts, err := s.q.EventCounts30d(ctx, toPg(oc.OrgID))
	if err != nil {
		return nil, err
	}
	events := map[string]int64{}
	for _, c := range counts {
		events[c.EventType] = c.N
	}
	return json.Marshal(map[string]any{
		"role": role, "org_id": oc.OrgID, "generated_at": time.Now().UTC(),
		"events_30d": events, "sections": map[string]any{},
	})
}

// snapshot composes one role's view: the 30d domain-event projection
// (analytics' own read model) + each wired section producer.
func (s *Service) snapshot(ctx context.Context, oc *tenant.OrgCtx, role string) (map[string]any, error) {
	counts, err := s.q.EventCounts30d(ctx, toPg(oc.OrgID))
	if err != nil {
		return nil, err
	}
	events := map[string]int64{}
	for _, c := range counts {
		events[c.EventType] = c.N
	}
	snap := map[string]any{
		"role": role, "org_id": oc.OrgID, "generated_at": time.Now().UTC(),
		"events_30d": events, "sections": map[string]any{},
	}
	secs := snap["sections"].(map[string]any)
	for name, fn := range s.sections {
		v, err := fn(ctx, oc, role)
		if err == nil && v != nil {
			secs[name] = v
		}
	}
	return snap, nil
}

// RefreshSnapshots — analytics:refresh_snapshots + projection_rebuild:
// rebuild every live org's four role snapshots; one DashboardSnapshotRefreshed
// per org (sourced=no — an internal maintenance signal).
func (s *Service) RefreshSnapshots(ctx context.Context) (int, error) {
	orgs, err := s.q.ListActiveOrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, oid := range orgs {
		orgID := fromPg(oid)
		tid, err := s.q.OrgTenantID(ctx, oid)
		if err != nil {
			return n, err
		}
		oc := &tenant.OrgCtx{OrgID: orgID, TenantID: fromPg(tid)}
		for _, role := range snapshotRoles {
			snap, err := s.snapshot(ctx, oc, role)
			if err != nil {
				return n, err
			}
			raw, _ := json.Marshal(snap)
			if err := s.q.UpsertSnapshot(ctx, db.UpsertSnapshotParams{
				OrgID: toPg(orgID), Role: role, Snapshot: raw}); err != nil {
				return n, err
			}
		}
		if err := s.withTx(ctx, func(q *db.Queries) error {
			return events.Emit(ctx, q, oc.TenantID, orgID, "DashboardSnapshotRefreshed",
				"organization", orgID, uuid.Nil, false,
				map[string]any{"org_id": orgID})
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------- health score ----------

// Health returns the latest computed score — components + formula version
// ride along (§19 transparency).
func (s *Service) Health(ctx context.Context, oc *tenant.OrgCtx) (db.HealthScores, error) {
	h, err := s.q.LatestHealthScore(ctx, toPg(oc.OrgID))
	if err != nil {
		return h, ErrNotFound
	}
	return h, nil
}

// ComputeHealthScores — analytics:health_score (monthly): v1 weights
// membership/attendance/finance/engagement sections 30/30/20/20; section
// producers return a 0-100 component score. KPI thresholds checked after
// each upsert (KPIThresholdCrossed on violation).
func (s *Service) ComputeHealthScores(ctx context.Context) (int, error) {
	period := time.Now().UTC().Format("2006-01")
	orgs, err := s.q.ListActiveOrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	weights := map[string]int{"membership": 30, "attendance": 30, "finance": 20, "engagement": 20}
	var n int
	for _, oid := range orgs {
		orgID := fromPg(oid)
		tid, err := s.q.OrgTenantID(ctx, oid)
		if err != nil {
			return n, err
		}
		oc := &tenant.OrgCtx{OrgID: orgID, TenantID: fromPg(tid)}
		components := map[string]any{"formula": "membership*.3+attendance*.3+finance*.2+engagement*.2"}
		score := 0
		for key, w := range weights {
			v := 0
			if fn, ok := s.sections["health."+key]; ok {
				if raw, err := fn(ctx, oc, key); err == nil {
					if f, ok := raw.(float64); ok {
						v = int(f)
					}
					if i, ok := raw.(int64); ok {
						v = int(i)
					}
				}
			}
			if v < 0 {
				v = 0
			}
			if v > 100 {
				v = 100
			}
			components[key] = v
			score += v * w / 100
		}
		raw, _ := json.Marshal(components)
		if err := s.q.UpsertHealthScore(ctx, db.UpsertHealthScoreParams{
			OrgID: toPg(orgID), Period: period, Score: int32(score),
			Components: raw, FormulaVersion: healthFormulaVersion}); err != nil {
			return n, err
		}
		if err := s.withTx(ctx, func(q *db.Queries) error {
			if err := events.Emit(ctx, q, oc.TenantID, orgID, "HealthScoreCalculated",
				"health_score", orgID, uuid.Nil, false,
				map[string]any{"org_id": orgID, "score": score, "period": period}); err != nil {
				return err
			}
			return s.kpiCheck(ctx, q, oc, components)
		}); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// kpiCheck evaluates kpi_definitions rows ({component,op,value} formula)
// against the fresh components — a breach emits KPIThresholdCrossed.
func (s *Service) kpiCheck(ctx context.Context, q *db.Queries, oc *tenant.OrgCtx, components map[string]any) error {
	defs, err := q.ListKpiDefs(ctx, toPg(oc.OrgID))
	if err != nil {
		return err
	}
	for _, d := range defs {
		var f struct {
			Component string  `json:"component"`
			Op        string  `json:"op"` // lt|gt
			Value     float64 `json:"value"`
		}
		if json.Unmarshal(d.Formula, &f) != nil || f.Component == "" {
			continue
		}
		raw, ok := components[f.Component]
		if !ok {
			continue
		}
		v, _ := raw.(int)
		breach := (f.Op == "lt" && float64(v) < f.Value) || (f.Op == "gt" && float64(v) > f.Value)
		if !breach {
			continue
		}
		if err := events.Emit(ctx, q, oc.TenantID, oc.OrgID, "KPIThresholdCrossed",
			"kpi_definition", fromPg(d.ID), uuid.Nil, false,
			map[string]any{"org_id": oc.OrgID, "kpi_key": d.Key, "value": v}); err != nil {
			return err
		}
	}
	return nil
}

// ---------- reports ----------

// ListRuns / RunReport — §19: reports are generated artifacts (document_id)
// recorded in report_runs with kind+period.
func (s *Service) ListRuns(ctx context.Context, oc *tenant.OrgCtx) ([]db.ReportRuns, error) {
	return s.q.ListReportRuns(ctx, toPg(oc.OrgID))
}

func (s *Service) GetRun(ctx context.Context, oc *tenant.OrgCtx, rid uuid.UUID) (db.ReportRuns, error) {
	r, err := s.q.GetReportRun(ctx, db.GetReportRunParams{ID: toPg(rid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return r, ErrNotFound
	}
	return r, nil
}

// RunReport builds the report document — dashboard snapshots + latest
// health + 30d projection — and lands it via the documents seam.
func (s *Service) RunReport(ctx context.Context, oc *tenant.OrgCtx, kind, period string, params json.RawMessage) (uuid.UUID, error) {
	if !reportKinds[kind] {
		return uuid.Nil, ErrBadValue
	}
	if period == "" {
		period = time.Now().UTC().Format("2006-01")
	}
	report := map[string]any{
		"kind": kind, "period": period, "org_id": oc.OrgID,
		"generated_at": time.Now().UTC(), "snapshots": map[string]json.RawMessage{},
	}
	snaps := report["snapshots"].(map[string]json.RawMessage)
	for _, role := range snapshotRoles {
		if row, err := s.q.GetSnapshot(ctx, db.GetSnapshotParams{
			OrganizationID: toPg(oc.OrgID), Role: role}); err == nil {
			snaps[role] = row.Snapshot
		}
	}
	if h, err := s.q.LatestHealthScore(ctx, toPg(oc.OrgID)); err == nil {
		report["health"] = map[string]any{"score": h.Score, "period": h.Period,
			"components": h.Components, "formula_version": h.FormulaVersion}
	}
	var docID uuid.UUID
	if s.archiveFn != nil {
		raw, _ := json.MarshalIndent(report, "", "  ")
		id, err := s.archiveFn(ctx, oc, kind+"-report-"+period, bytes.NewReader(raw))
		if err != nil {
			return uuid.Nil, err
		}
		docID = id
	}
	var rid uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		id, err := q.InsertReportRun(ctx, db.InsertReportRunParams{
			OrgID: toPg(oc.OrgID), Kind: kind, Period: pgText(period),
			Params: params, DocumentID: toPg(docID), GeneratedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		rid = fromPg(id)
		// only the two catalogued report events exist — custom/handover
		// runs record the row without a mislabeled event name.
		var ev string
		switch kind {
		case reportMonthly:
			ev = "MonthlyReportGenerated"
		case reportAnnual:
			ev = "AnnualReportGenerated"
		}
		if ev == "" {
			return nil
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, ev,
			"report_run", rid, oc.UserID, false,
			map[string]any{"report_id": rid, "org_id": oc.OrgID, "kind": kind, "period": period})
	})
	return rid, err
}

// MonthlyReports — analytics:monthly_report: one monthly run per org per
// period (dedupe on ReportRunExists — re-runs are safe).
func (s *Service) MonthlyReports(ctx context.Context) (int, error) {
	period := time.Now().UTC().Format("2006-01")
	orgs, err := s.q.ListActiveOrgIDs(ctx)
	if err != nil {
		return 0, err
	}
	var n int
	for _, oid := range orgs {
		exists, err := s.q.ReportRunExists(ctx, db.ReportRunExistsParams{
			OrganizationID: oid, Kind: reportMonthly, Period: pgText(period)})
		if err != nil || exists {
			if err != nil {
				return n, err
			}
			continue
		}
		tid, err := s.q.OrgTenantID(ctx, oid)
		if err != nil {
			return n, err
		}
		oc := &tenant.OrgCtx{OrgID: fromPg(oid), TenantID: fromPg(tid)}
		if _, err := s.RunReport(ctx, oc, reportMonthly, period, nil); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// ---------- consolidated (Business cap) + /me ----------

// Consolidated — multi-branch roll-up, Business-tier only
// (capability.consolidation; test 4 → capability_required otherwise).
func (s *Service) Consolidated(ctx context.Context, oc *tenant.OrgCtx) (any, error) {
	if s.capFn != nil && !s.capFn(ctx, oc.TenantID, "capability.consolidation") {
		return nil, ErrCapability
	}
	// Roll up the four role snapshots + health + the per-branch blocks
	// the wired producer supplies (§19: consolidation IS the multi-branch
	// roll-up — the branch seam is the point of this route).
	out := map[string]any{"org_id": oc.OrgID, "roles": map[string]json.RawMessage{}}
	roles := out["roles"].(map[string]json.RawMessage)
	for _, role := range snapshotRoles {
		if row, err := s.q.GetSnapshot(ctx, db.GetSnapshotParams{
			OrganizationID: toPg(oc.OrgID), Role: role}); err == nil {
			roles[role] = row.Snapshot
		}
	}
	if h, err := s.q.LatestHealthScore(ctx, toPg(oc.OrgID)); err == nil {
		out["health"] = map[string]any{"score": h.Score, "period": h.Period}
	}
	if s.branchFn != nil {
		branches, err := s.branchFn(ctx, oc)
		if err != nil {
			return nil, err
		}
		out["branches"] = branches
	}
	return out, nil
}

// Me — /me/analytics via the injected member-data seam.
func (s *Service) Me(ctx context.Context, userID uuid.UUID) (any, error) {
	if s.myFn == nil {
		return map[string]any{}, nil
	}
	return s.myFn(ctx, userID)
}
