// Ref: oga9-engineering/sections/18-assets.md — §18 asset routes.
package handler

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/assets"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

type AssetsHandler struct {
	svc *assets.Service
}

func NewAssetsHandler(s *assets.Service) *AssetsHandler {
	return &AssetsHandler{svc: s}
}

func assetErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, assets.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, assets.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, assets.ErrBadState):
		httpx.WriteErr(w, 409, "bad_asset_state", "asset is not in a state for that action")
	case errors.Is(err, assets.ErrCustodian):
		httpx.WriteErr(w, 422, "custodian_required", "exactly one of holder_member_id / holder_unit_id required")
	default:
		log.Printf("assets: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

func assetID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "asset_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return uuid.Nil, false
	}
	return id, true
}

type assetReq struct {
	Category    string    `json:"category"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Identifier  string    `json:"identifier"`
	Condition   string    `json:"condition"`
	AcquiredAt  time.Time `json:"acquired_at"`
	CostMinor   int64     `json:"cost_minor"`
	Currency    string    `json:"currency"`
	UnitID      uuid.UUID `json:"unit_id"`
}

func (h *AssetsHandler) List(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows, err := h.svc.List(r.Context(), middleware.OrgCtx(r.Context()), q.Get("state"), q.Get("category"))
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *AssetsHandler) Register(w http.ResponseWriter, r *http.Request) {
	var req assetReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Category == "" || req.Name == "" {
		httpx.WriteErr(w, 400, "validation_failed", "category and name required")
		return
	}
	id, err := h.svc.Register(r.Context(), middleware.OrgCtx(r.Context()),
		req.Category, req.Name, req.Description, req.Identifier, req.Condition,
		req.AcquiredAt, req.CostMinor, req.Currency, req.UnitID)
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"asset_id": id})
}

func (h *AssetsHandler) Get(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	a, err := h.svc.Get(r.Context(), middleware.OrgCtx(r.Context()), aid)
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, a)
}

func (h *AssetsHandler) Patch(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	var req assetReq
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.Patch(r.Context(), middleware.OrgCtx(r.Context()), aid,
		req.Name, req.Description, req.Condition, req.UnitID); err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"patched": true})
}

type assignReq struct {
	HolderMemberID uuid.UUID `json:"holder_member_id"`
	HolderUnitID   uuid.UUID `json:"holder_unit_id"`
}

func (h *AssetsHandler) Assign(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	var req assignReq
	if err := httpx.Decode(w, r, &req); err != nil {
		return
	}
	id, err := h.svc.Assign(r.Context(), middleware.OrgCtx(r.Context()), aid, req.HolderMemberID, req.HolderUnitID)
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"custody_id": id})
}

func (h *AssetsHandler) Return(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Return(r.Context(), middleware.OrgCtx(r.Context()), aid); err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"returned": true})
}

type maintenanceReq struct {
	Kind      string    `json:"kind"`
	Detail    string    `json:"detail"`
	CostMinor int64     `json:"cost_minor"`
	Currency  string    `json:"currency"`
	NextDue   time.Time `json:"next_due"`
}

func (h *AssetsHandler) Maintenance(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	var req maintenanceReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Kind == "" {
		httpx.WriteErr(w, 400, "validation_failed", "kind required")
		return
	}
	id, err := h.svc.LogMaintenance(r.Context(), middleware.OrgCtx(r.Context()), aid,
		req.Kind, req.Detail, req.CostMinor, req.Currency, req.NextDue)
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"record_id": id})
}

type disposeReq struct {
	SoldAmountMinor int64  `json:"sold_amount_minor"`
	Currency        string `json:"currency"`
}

func (h *AssetsHandler) Dispose(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	var req disposeReq
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.Dispose(r.Context(), middleware.OrgCtx(r.Context()), aid,
		req.SoldAmountMinor, req.Currency); err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "disposed"})
}

func (h *AssetsHandler) ListAudits(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListAudits(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *AssetsHandler) StartAudit(w http.ResponseWriter, r *http.Request) {
	id, err := h.svc.StartAudit(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"audit_id": id})
}

type auditCompleteReq struct {
	Findings json.RawMessage `json:"findings"`
}

func (h *AssetsHandler) CompleteAudit(w http.ResponseWriter, r *http.Request) {
	auditID, err := uuid.Parse(chi.URLParam(r, "audit_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req auditCompleteReq
	_ = httpx.Decode(w, r, &req)
	if err := h.svc.CompleteAudit(r.Context(), middleware.OrgCtx(r.Context()), auditID, req.Findings); err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"state": "completed"})
}

type credentialReq struct {
	Kind      string `json:"kind"`
	Label     string `json:"label"`
	Reference string `json:"reference"`
	VaultRef  string `json:"vault_ref"`
}

func (h *AssetsHandler) ListCredentials(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	rows, err := h.svc.ListCredentials(r.Context(), middleware.OrgCtx(r.Context()), aid)
	if err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

func (h *AssetsHandler) SaveCredential(w http.ResponseWriter, r *http.Request) {
	aid, ok := assetID(w, r)
	if !ok {
		return
	}
	var req credentialReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Kind == "" || req.Label == "" {
		httpx.WriteErr(w, 400, "validation_failed", "kind, label and reference required")
		return
	}
	if err := h.svc.SaveCredential(r.Context(), middleware.OrgCtx(r.Context()), aid,
		req.Kind, req.Label, req.Reference, req.VaultRef); err != nil {
		assetErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"saved": true})
}
