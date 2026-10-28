// Ref: oga9-engineering/sections/10-organization-core.md (ADR-022)
// Custom fields routes: groups + fields CRUD, merged /fields/schema for
// form rendering, entity-scoped /fields/values writer.
package handler

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
	"github.com/jedafe/oga9/internal/tenant"
)

type FieldsHandler struct{ svc *tenant.Service }

func NewFieldsHandler(svc *tenant.Service) *FieldsHandler { return &FieldsHandler{svc: svc} }

// Groups/Fields list endpoints surface through the same tier-filtered schema
// the form renderer consumes — admin-only definitions don't leak to members
// (the schema cache holds the full layout; tier filters per request).
func (h *FieldsHandler) Groups(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.FieldSchema(r.Context(), middleware.OrgCtx(r.Context()),
		r.URL.Query().Get("entity_type"))
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out["groups"])
}

type fieldGroupReq struct {
	EntityType string `json:"entity_type"`
	Name       string `json:"name"`
	Slug       string `json:"slug"`
	SortOrder  int32  `json:"sort_order"`
	Visibility string `json:"visibility"`
}

func (h *FieldsHandler) CreateGroup(w http.ResponseWriter, r *http.Request) {
	var req fieldGroupReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if req.Visibility == "" {
		req.Visibility = "admin"
	}
	id, err := h.svc.CreateFieldGroup(r.Context(), middleware.OrgCtx(r.Context()),
		req.EntityType, req.Name, req.Slug, req.SortOrder, req.Visibility)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *FieldsHandler) PatchGroup(w http.ResponseWriter, r *http.Request) {
	gid, err := uuid.Parse(chi.URLParam(r, "group_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req struct {
		Name       string `json:"name"`
		SortOrder  int32  `json:"sort_order"`
		Visibility string `json:"visibility"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.Name == "" {
		httpx.WriteErr(w, 400, "validation_failed", "name required")
		return
	}
	if err := h.svc.UpdateFieldGroup(r.Context(), middleware.OrgCtx(r.Context()),
		gid, req.Name, req.SortOrder, req.Visibility); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": gid})
}

func (h *FieldsHandler) DeleteGroup(w http.ResponseWriter, r *http.Request) {
	gid, err := uuid.Parse(chi.URLParam(r, "group_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.DeleteFieldGroup(r.Context(), middleware.OrgCtx(r.Context()), gid); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"deleted": true})
}

func (h *FieldsHandler) Fields(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.FieldSchema(r.Context(), middleware.OrgCtx(r.Context()),
		r.URL.Query().Get("entity_type"))
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out["fields"])
}

type fieldReq struct {
	EntityType string          `json:"entity_type"`
	GroupID    uuid.UUID       `json:"group_id"`
	Key        string          `json:"key"`
	Label      string          `json:"label"`
	HelpText   string          `json:"help_text"`
	Kind       string          `json:"kind"`
	Required   bool            `json:"required"`
	Options    json.RawMessage `json:"options"`
	Validation json.RawMessage `json:"validation"`
	SortOrder  int32           `json:"sort_order"`
	Visibility string          `json:"visibility"`
}

func (h *FieldsHandler) CreateField(w http.ResponseWriter, r *http.Request) {
	var req fieldReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	if req.Visibility == "" {
		req.Visibility = "admin"
	}
	id, err := h.svc.CreateField(r.Context(), middleware.OrgCtx(r.Context()),
		req.EntityType, req.GroupID, req.Key, req.Label, req.HelpText, req.Kind,
		req.Required, req.Options, req.Validation, req.SortOrder, req.Visibility)
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"id": id})
}

func (h *FieldsHandler) PatchField(w http.ResponseWriter, r *http.Request) {
	fid, err := uuid.Parse(chi.URLParam(r, "field_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req fieldReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Label == "" {
		httpx.WriteErr(w, 400, "validation_failed", "label required")
		return
	}
	if err := h.svc.UpdateField(r.Context(), middleware.OrgCtx(r.Context()), fid,
		req.GroupID, req.Label, req.HelpText, req.Required, req.Options,
		req.Validation, req.SortOrder, req.Visibility); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"id": fid})
}

func (h *FieldsHandler) DeleteField(w http.ResponseWriter, r *http.Request) {
	fid, err := uuid.Parse(chi.URLParam(r, "field_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	if err := h.svc.DeleteField(r.Context(), middleware.OrgCtx(r.Context()), fid); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"deleted": true})
}

func (h *FieldsHandler) Schema(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.FieldSchema(r.Context(), middleware.OrgCtx(r.Context()),
		r.URL.Query().Get("entity_type"))
	if err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

// SetValues — PATCH /orgs/{id}/fields/values {entity_type, entity_id, values{key: v}}.
// Self-write covers member-visible fields on the caller's own membership;
// everything else requires org.fields.manage (checked in the service).
func (h *FieldsHandler) SetValues(w http.ResponseWriter, r *http.Request) {
	var req struct {
		EntityType string                     `json:"entity_type"`
		EntityID   uuid.UUID                  `json:"entity_id"`
		Values     map[string]json.RawMessage `json:"values"`
	}
	if err := httpx.Decode(w, r, &req); err != nil || req.EntityID == uuid.Nil || len(req.Values) == 0 {
		httpx.WriteErr(w, 400, "validation_failed", "entity_type, entity_id and values required")
		return
	}
	if err := h.svc.SetFieldValues(r.Context(), middleware.OrgCtx(r.Context()),
		req.EntityType, req.EntityID, req.Values); err != nil {
		orgErr(w, err)
		return
	}
	httpx.WriteData(w, 200, map[string]any{"set": len(req.Values)})
}
