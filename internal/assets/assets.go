// Ref: oga9-engineering/sections/18-assets.md — registry, term-scoped
// custody, maintenance, audits, credential refs. Disposal sales post to
// finance via the AssetDisposed consumer (never off-ledger).
package assets

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
	"github.com/jedafe/oga9/internal/tenant"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrBadValue  = errors.New("bad value")
	ErrBadState  = errors.New("bad_asset_state")
	ErrCustodian = errors.New("custodian_required")
)

var assetCategories = map[string]bool{"building": true, "vehicle": true, "equipment": true,
	"inventory": true, "digital": true, "bank_account": true, "domain": true,
	"social_account": true, "seal": true, "archive": true}
var assetConditions = map[string]bool{"new": true, "good": true, "fair": true, "poor": true, "disposed": true}
var maintenanceKinds = map[string]bool{"service": true, "repair": true, "inspection": true}
var credentialKinds = map[string]bool{"domain": true, "email": true, "social": true, "bank": true, "other": true}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func fromPg(u pgtype.UUID) uuid.UUID { return uuid.UUID(u.Bytes) }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }
func pgDate(t time.Time) pgtype.Date { return pgtype.Date{Time: t, Valid: !t.IsZero()} }
func nInt(v int64) pgtype.Int8       { return pgtype.Int8{Int64: v, Valid: v != 0} }

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
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

// ---------- registry ----------

// Register — §18: every asset enters the registry; custody follows.
func (s *Service) Register(ctx context.Context, oc *tenant.OrgCtx, category, name, description,
	identifier, condition string, acquiredAt time.Time, costMinor int64, currency string,
	unitID uuid.UUID) (uuid.UUID, error) {
	if !assetCategories[category] || name == "" {
		return uuid.Nil, ErrBadValue
	}
	if condition == "" {
		condition = "good"
	}
	if !assetConditions[condition] {
		return uuid.Nil, ErrBadValue
	}
	var id uuid.UUID
	err := s.withTx(ctx, func(q *db.Queries) error {
		aid, err := q.CreateAsset(ctx, db.CreateAssetParams{
			OrgID: toPg(oc.OrgID), Category: category, Name: name,
			Description: pgText(description), Identifier: pgText(identifier),
			Condition: condition, AcquiredAt: pgDate(acquiredAt),
			CostMinor: nInt(costMinor), Currency: pgText(currency), UnitID: toPg(unitID)})
		if err != nil {
			return err
		}
		id = fromPg(aid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AssetRegistered",
			"asset", id, oc.UserID, true,
			map[string]any{"asset_id": id, "category": category, "name": name})
	})
	return id, err
}

func (s *Service) List(ctx context.Context, oc *tenant.OrgCtx, state, category string) ([]db.Assets, error) {
	return s.q.ListAssets(ctx, db.ListAssetsParams{
		OrgID: toPg(oc.OrgID), State: pgText(state), Category: pgText(category)})
}

func (s *Service) Get(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) (db.Assets, error) {
	a, err := s.q.GetAsset(ctx, db.GetAssetParams{ID: toPg(aid), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return a, ErrNotFound
	}
	return a, nil
}

// Patch — registry fields only; disposal/state changes go through their
// own guarded transitions.
func (s *Service) Patch(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID,
	name, description, condition string, unitID uuid.UUID) error {
	if _, err := s.Get(ctx, oc, aid); err != nil {
		return err
	}
	if condition != "" && !assetConditions[condition] {
		return ErrBadValue
	}
	n, err := s.q.PatchAsset(ctx, db.PatchAssetParams{
		ID: toPg(aid), Name: pgText(name), Description: pgText(description),
		Condition: pgText(condition), UnitID: toPg(unitID)})
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrBadState // disposed assets are frozen
	}
	return nil
}

// ---------- custody (term-scoped, Rule 21) ----------

// Assign — one open custody at a time; binds the org's current term so a
// term close can flag what never came back.
func (s *Service) Assign(ctx context.Context, oc *tenant.OrgCtx, aid,
	holderMember, holderUnit uuid.UUID) (uuid.UUID, error) {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return uuid.Nil, err
	}
	if a.State != "active" && a.State != "in_repair" {
		return uuid.Nil, ErrBadState
	}
	if (holderMember == uuid.Nil) == (holderUnit == uuid.Nil) {
		return uuid.Nil, ErrCustodian // exactly one holder kind
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		// close any still-open custody before reassignment
		if cur, err := q.CurrentCustody(ctx, a.ID); err == nil {
			if _, err := q.ReturnCustody(ctx, cur.ID); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		termID, _ := q.CurrentTermID(ctx, toPg(oc.OrgID)) // custody binds the term
		cid, err := q.AssignCustody(ctx, db.AssignCustodyParams{
			AssetID: a.ID, HolderMemberID: toPg(holderMember), HolderUnitID: toPg(holderUnit),
			TermID: termID, AssignedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		id = fromPg(cid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AssetAssigned",
			"asset", fromPg(a.ID), oc.UserID, true,
			map[string]any{"asset_id": fromPg(a.ID), "custody_id": id,
				"holder_member_id": holderMember, "holder_unit_id": holderUnit})
	})
	return id, err
}

// Return — closes the open custody (idempotent: already-returned is fine).
func (s *Service) Return(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) error {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		cur, err := q.CurrentCustody(ctx, a.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := q.ReturnCustody(ctx, cur.ID); err != nil {
			return err
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AssetReturned",
			"asset", fromPg(a.ID), oc.UserID, false,
			map[string]any{"asset_id": fromPg(a.ID), "custody_id": fromPg(cur.ID)})
	})
}

// ---------- maintenance ----------

func (s *Service) LogMaintenance(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID,
	kind, detail string, costMinor int64, currency string, nextDue time.Time) (uuid.UUID, error) {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return uuid.Nil, err
	}
	if !maintenanceKinds[kind] {
		return uuid.Nil, ErrBadValue
	}
	if a.State == "disposed" {
		return uuid.Nil, ErrBadState
	}
	var id uuid.UUID
	err = s.withTx(ctx, func(q *db.Queries) error {
		rid, err := q.LogMaintenance(ctx, db.LogMaintenanceParams{
			AssetID: a.ID, Kind: kind, Detail: pgText(detail), CostMinor: nInt(costMinor),
			Currency: pgText(currency), NextDue: pgDate(nextDue), LoggedBy: toPg(oc.MembershipID)})
		if err != nil {
			return err
		}
		id = fromPg(rid)
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AssetMaintenanceLogged",
			"maintenance_record", id, oc.UserID, false,
			map[string]any{"record_id": id, "asset_id": fromPg(a.ID), "kind": kind})
	})
	return id, err
}

func (s *Service) MaintenanceHistory(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) ([]db.MaintenanceRecords, error) {
	if _, err := s.Get(ctx, oc, aid); err != nil {
		return nil, err
	}
	return s.q.ListMaintenance(ctx, toPg(aid))
}

// ---------- dispose ----------

// Dispose — approval rides on the org.assets.dispose node; a sale posts a
// credit ledger entry via the finance AssetDisposed consumer (Rule 19).
func (s *Service) Dispose(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID,
	soldMinor int64, currency string) error {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.SetAssetState(ctx, db.SetAssetStateParams{
			ID: a.ID, State: "disposed", Condition: "disposed", State_2: a.State})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState // already disposed/missing
		}
		if cur, err := q.CurrentCustody(ctx, a.ID); err == nil {
			if _, err := q.ReturnCustody(ctx, cur.ID); err != nil {
				return err
			}
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AssetDisposed",
			"asset", fromPg(a.ID), oc.UserID, true,
			map[string]any{"asset_id": fromPg(a.ID), "sold_amount_minor": soldMinor,
				"currency": currency, "posted_by": oc.MembershipID})
	})
}

// ---------- audits ----------

func (s *Service) StartAudit(ctx context.Context, oc *tenant.OrgCtx) (uuid.UUID, error) {
	id, err := s.q.CreateAudit(ctx, db.CreateAuditParams{
		OrgID: toPg(oc.OrgID), StartedBy: toPg(oc.MembershipID)})
	return fromPg(id), err
}

func (s *Service) ListAudits(ctx context.Context, oc *tenant.OrgCtx) ([]db.AssetAudits, error) {
	return s.q.ListAudits(ctx, toPg(oc.OrgID))
}

// CompleteAudit — findings land; missing items stay visible in findings
// until a follow-up audit (test 4: state completes with findings).
func (s *Service) CompleteAudit(ctx context.Context, oc *tenant.OrgCtx, auditID uuid.UUID, findings json.RawMessage) error {
	a, err := s.q.GetAudit(ctx, db.GetAuditParams{ID: toPg(auditID), OrganizationID: toPg(oc.OrgID)})
	if err != nil {
		return ErrNotFound
	}
	return s.withTx(ctx, func(q *db.Queries) error {
		n, err := q.CompleteAudit(ctx, db.CompleteAuditParams{ID: a.ID, Findings: findings})
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrBadState // already completed
		}
		return events.Emit(ctx, q, oc.TenantID, oc.OrgID, "AssetAuditCompleted",
			"asset_audit", fromPg(a.ID), oc.UserID, false,
			map[string]any{"audit_id": fromPg(a.ID)})
	})
}

// ---------- digital credentials (refs only — never secrets) ----------

// SaveCredential stores an ownership reference; vault_ref points at the
// secrets manager — plaintext never lands here (§18 rule).
func (s *Service) SaveCredential(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID,
	kind, label, reference, vaultRef string) error {
	a, err := s.Get(ctx, oc, aid)
	if err != nil {
		return err
	}
	if !credentialKinds[kind] || label == "" || reference == "" {
		return ErrBadValue
	}
	return s.q.UpsertCredential(ctx, db.UpsertCredentialParams{
		AssetID: a.ID, Kind: kind, Label: label, Reference: reference, VaultRef: pgText(vaultRef)})
}

func (s *Service) ListCredentials(ctx context.Context, oc *tenant.OrgCtx, aid uuid.UUID) ([]db.DigitalCredentials, error) {
	if _, err := s.Get(ctx, oc, aid); err != nil {
		return nil, err
	}
	return s.q.ListCredentials(ctx, toPg(aid))
}

// ---------- sweeps ----------

// FlagUnreturned — assets:handover_sweep + TermEnded consumer: custody
// still open at term end is flagged to the incoming EXCO
// (CustodyHandoverFlagged is sourced → member_notify delivers it).
// Dedupes per custody row via the emitted event (at-least-once safe).
func (s *Service) FlagUnreturned(ctx context.Context, termID uuid.UUID) (int, error) {
	rows, err := s.q.UnreturnedCustodyInTerm(ctx, toPg(termID))
	if err != nil {
		return 0, err
	}
	var n int
	for _, c := range rows {
		err := s.withTx(ctx, func(q *db.Queries) error {
			tid, err := q.OrgTenantID(ctx, c.OrganizationID)
			if err != nil {
				return err
			}
			return events.Emit(ctx, q, fromPg(tid), fromPg(c.OrganizationID),
				"CustodyHandoverFlagged", "custody_assignment", fromPg(c.ID), uuid.Nil, true,
				map[string]any{"custody_id": fromPg(c.ID), "asset_id": fromPg(c.AssetID),
					"asset_name": c.AssetName, "term_id": termID,
					"member_id": fromPg(c.HolderMemberID)})
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// TermEndedConsumer — TermEnded → flag that term's open custody.
func (s *Service) TermEndedConsumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if e.Type != "TermEnded" {
		return nil
	}
	termStr, _ := e.Payload["term_id"].(string)
	termID, err := uuid.Parse(termStr)
	if err != nil || termID == uuid.Nil {
		return nil
	}
	rows, err := q.UnreturnedCustodyInTerm(ctx, toPg(termID))
	if err != nil {
		return err
	}
	for _, c := range rows {
		if err := events.Emit(ctx, q, e.TenantID, fromPg(c.OrganizationID),
			"CustodyHandoverFlagged", "custody_assignment", fromPg(c.ID), uuid.Nil, true,
			map[string]any{"custody_id": fromPg(c.ID), "asset_id": fromPg(c.AssetID),
				"asset_name": c.AssetName, "term_id": termID,
				"member_id": fromPg(c.HolderMemberID)}); err != nil {
			return err
		}
	}
	return nil
}

// MaintenanceDueSweep — assets:maintenance_due (weekly): records whose
// next_due lands inside the window emit MaintenanceDue (sourced → the
// custodian's notification fires).
func (s *Service) MaintenanceDueSweep(ctx context.Context, windowDays int) (int, error) {
	rows, err := s.q.MaintenanceDue(ctx, int32(windowDays))
	if err != nil {
		return 0, err
	}
	var n int
	for _, r := range rows {
		err := s.withTx(ctx, func(q *db.Queries) error {
			tid, err := q.OrgTenantID(ctx, r.OrganizationID)
			if err != nil {
				return err
			}
			member := fromPg(r.LoggedBy)
			if cur, err := q.CurrentCustody(ctx, r.AssetID); err == nil && cur.HolderMemberID.Valid {
				member = fromPg(cur.HolderMemberID) // notify the custodian, not the logger
			}
			return events.Emit(ctx, q, fromPg(tid), fromPg(r.OrganizationID),
				"MaintenanceDue", "maintenance_record", fromPg(r.ID), uuid.Nil, true,
				map[string]any{"record_id": fromPg(r.ID), "asset_id": fromPg(r.AssetID),
					"asset_name": r.AssetName, "member_id": member,
					"due_on": r.NextDue.Time.Format("2006-01-02")})
		})
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
