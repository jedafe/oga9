// Ref: oga9-engineering/sections/04-tenancy-organizations.md, 10-organization-core.md
package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tenant"
)

type OrgHandler struct{ svc *tenant.Service }

func NewOrgHandler(svc *tenant.Service) *OrgHandler { return &OrgHandler{svc: svc} }

func orgErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, tenant.ErrOrgNotFound):
		httpx.WriteErr(w, 404, "org_not_found", "organization not found")
	case errors.Is(err, tenant.ErrNotMember):
		httpx.WriteErr(w, 403, "not_a_member", "not a member of this organization")
	case errors.Is(err, tenant.ErrTemplateNotFound):
		httpx.WriteErr(w, 400, "template_not_found", "unknown organization template")
	case errors.Is(err, tenant.ErrSlugTaken):
		httpx.WriteErr(w, 409, "slug_taken", "org slug already in use for this tenant")
	case errors.Is(err, tenant.ErrLimitExceeded):
		httpx.WriteErr(w, 402, "limit_exceeded", "organization limit reached for this plan")
	case errors.Is(err, tenant.ErrTenantRequired):
		httpx.WriteErr(w, 400, "tenant_required", "you administer multiple tenants — send tenant_id")
	case errors.Is(err, tenant.ErrNoTenant):
		httpx.WriteErr(w, 403, "forbidden", "no admin rights on the requested tenant")
	case errors.Is(err, tenant.ErrUnitNotFound):
		httpx.WriteErr(w, 404, "unit_not_found", "unit not found")
	case errors.Is(err, tenant.ErrUnitDependents):
		httpx.WriteErr(w, 409, "unit_has_dependents", "unit has active child units")
	case errors.Is(err, tenant.ErrUnitCycle):
		httpx.WriteErr(w, 400, "unit_cycle", "reparenting would create a cycle")
	case errors.Is(err, tenant.ErrBadUnitType):
		httpx.WriteErr(w, 400, "invalid_unit_type", "type must be hq|branch|chapter|committee|wing")
	case errors.Is(err, tenant.ErrUnknownSetting):
		httpx.WriteErr(w, 400, "unknown_setting_key", "key not in the settings catalog")
	case errors.Is(err, tenant.ErrBadValue):
		httpx.WriteErr(w, 400, "validation_failed", "value must be valid json")
	case errors.Is(err, tenant.ErrInviteNoContact):
		httpx.WriteErr(w, 400, "validation_failed", "email or phone required")
	case errors.Is(err, tenant.ErrInviteNotFound):
		httpx.WriteErr(w, 404, "invitation_not_found", "no pending invitation with that id")
	case errors.Is(err, tenant.ErrForbidden):
		httpx.WriteErr(w, 403, "forbidden", "invitation is not addressed to this account")
	case errors.Is(err, tenant.ErrInviteDead):
		httpx.WriteErr(w, 410, "invitation_expired", "invitation is expired, revoked, or used")
	default:
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

type createOrgReq struct {
	Name         string    `json:"name"`
	Slug         string    `json:"slug"`
	TemplateSlug string    `json:"template_slug"`
	OrgType      string    `json:"org_type"`
	Country      string    `json:"country"`
	City         string    `json:"city"`
	BillingEmail string    `json:"billing_email"`
	TenantID     uuid.UUID `json:"tenant_id"`
}

func (h *OrgHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createOrgReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Name == "" || req.Slug == "" || req.TemplateSlug == "" {
		httpx.WriteErr(w, 400, "validation_failed", "name, slug and template_slug required")
		return
	}
	org, err := h.svc.CreateOrg(r.Context(), middleware.UserID(r.Context()),
		req.Name, req.Slug, req.TemplateSlug, req.OrgType, req.Country, req.City, req.BillingEmail, req.TenantID)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 201, org)
}

func (h *OrgHandler) Templates(w http.ResponseWriter, r *http.Request) {
	ts, err := h.svc.ListTemplates(r.Context())
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, ts)
}

func (h *OrgHandler) MyOrgs(w http.ResponseWriter, r *http.Request) {
	orgs, err := h.svc.ListUserOrgs(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, orgs)
}

func (h *OrgHandler) Get(w http.ResponseWriter, r *http.Request) {
	org, err := h.svc.GetOrg(r.Context(), middleware.OrgCtx(r.Context()).OrgID)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, org)
}

func (h *OrgHandler) Patch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name, Country, City string
		IsPublicListed      bool `json:"is_public_listed"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Name == "" {
		httpx.WriteErr(w, 400, "validation_failed", "name required")
		return
	}
	if err := h.svc.UpdateOrgProfile(r.Context(), middleware.OrgCtx(r.Context()),
		req.Name, req.Country, req.City, req.IsPublicListed); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"updated": true})
}

func (h *OrgHandler) Structure(w http.ResponseWriter, r *http.Request) {
	h.Units(w, r)
}

func (h *OrgHandler) Units(w http.ResponseWriter, r *http.Request) {
	units, err := h.svc.ListUnits(r.Context(), middleware.OrgCtx(r.Context()).OrgID)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, units)
}

type unitReq struct {
	ParentID   uuid.UUID `json:"parent_unit_id"`
	Type       string    `json:"type"`
	LevelLabel string    `json:"level_label"`
	Name       string    `json:"name"`
	Slug       string    `json:"slug"`
}

func (h *OrgHandler) CreateUnit(w http.ResponseWriter, r *http.Request) {
	var req unitReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Name == "" || req.Slug == "" || req.Type == "" {
		httpx.WriteErr(w, 400, "validation_failed", "name, slug and type required")
		return
	}
	id, err := h.svc.CreateUnit(r.Context(), middleware.OrgCtx(r.Context()),
		req.ParentID, req.Type, req.LevelLabel, req.Name, req.Slug)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *OrgHandler) UpdateUnit(w http.ResponseWriter, r *http.Request) {
	uid, err := uuid.Parse(chi.URLParam(r, "unit_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "bad unit id")
		return
	}
	var req unitReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Name == "" || req.Type == "" {
		httpx.WriteErr(w, 400, "validation_failed", "name and type required")
		return
	}
	if err := h.svc.UpdateUnit(r.Context(), middleware.OrgCtx(r.Context()),
		uid, req.ParentID, req.Type, req.LevelLabel, req.Name); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"updated": true})
}

func (h *OrgHandler) ArchiveUnit(w http.ResponseWriter, r *http.Request) {
	uid, err := uuid.Parse(chi.URLParam(r, "unit_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "bad unit id")
		return
	}
	if err := h.svc.ArchiveUnit(r.Context(), middleware.OrgCtx(r.Context()), uid); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"archived": true})
}

func (h *OrgHandler) Settings(w http.ResponseWriter, r *http.Request) {
	ss, err := h.svc.ListSettings(r.Context(), middleware.OrgCtx(r.Context()).OrgID)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, ss)
}

func (h *OrgHandler) SetSetting(w http.ResponseWriter, r *http.Request) {
	var req map[string]json.RawMessage
	if err := httpx.Decode(w, r, &req); err != nil || len(req) != 1 {
		httpx.WriteErr(w, 400, "validation_failed", "single {key: value} pair expected")
		return
	}
	for k, v := range req {
		if err := h.svc.SetSetting(r.Context(), middleware.OrgCtx(r.Context()), k, v); err != nil {
			orgErr(w, err)
			return
		}
	}
	httpx.WriteData(w, 200, map[string]any{"updated": true})
}

type inviteReq struct {
	Email        string `json:"email"`
	Phone        string `json:"phone"`
	MemberNoHint string `json:"member_no_hint"`
	Category     string `json:"category"`
}

func (h *OrgHandler) Invite(w http.ResponseWriter, r *http.Request) {
	var req inviteReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "bad request")
		return
	}
	if req.Category == "" {
		req.Category = "floor"
	}
	inv, err := h.svc.CreateInvitation(r.Context(), middleware.OrgCtx(r.Context()),
		req.Email, req.Phone, req.MemberNoHint, req.Category)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 201, inv)
}

func (h *OrgHandler) Invitations(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.ListInvitations(r.Context(), middleware.OrgCtx(r.Context()).OrgID)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, list)
}

func (h *OrgHandler) RevokeInvitation(w http.ResponseWriter, r *http.Request) {
	iid, err := uuid.Parse(chi.URLParam(r, "invitation_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "bad invitation id")
		return
	}
	if err := h.svc.RevokeInvitation(r.Context(), middleware.OrgCtx(r.Context()), iid); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"revoked": true})
}

// Accept is a sessioned public route — the caller logs in (or registers)
// then redeems the invite token.
func (h *OrgHandler) Accept(w http.ResponseWriter, r *http.Request) {
	tok, err := uuid.Parse(chi.URLParam(r, "token"))
	if err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "bad token")
		return
	}
	orgID, err := h.svc.AcceptInvitation(r.Context(), middleware.UserID(r.Context()), tok)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"organization_id": orgID})
}

func (h *OrgHandler) Branding(w http.ResponseWriter, r *http.Request) {
	var b tenant.Branding
	if err := httpx.Decode(w, r, &b); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "bad request")
		return
	}
	if err := h.svc.SetBranding(r.Context(), middleware.OrgCtx(r.Context()), b); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"updated": true})
}

func (h *OrgHandler) Directory(w http.ResponseWriter, r *http.Request) {
	orgs, err := h.svc.Directory(r.Context())
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, orgs)
}
