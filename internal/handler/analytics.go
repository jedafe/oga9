// Ref: oga9-engineering/sections/19-analytics.md — §19 analytics routes.
package handler

import (
	"errors"
	"io"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jedafe/oga9/internal/analytics"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

type AnalyticsHandler struct {
	svc *analytics.Service
}

func NewAnalyticsHandler(s *analytics.Service) *AnalyticsHandler {
	return &AnalyticsHandler{svc: s}
}

func analyticsErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, analytics.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "resource not found")
	case errors.Is(err, analytics.ErrBadValue):
		httpx.WriteErr(w, 422, "bad_value", "required field missing or invalid")
	case errors.Is(err, analytics.ErrCapability):
		httpx.WriteErr(w, 403, "capability_required", "plan does not include consolidation")
	default:
		log.Printf("analytics: %v", err)
		httpx.WriteErr(w, 500, "internal_error", "request failed")
	}
}

// Dashboard — the viewer's role resolves server-side from their
// permission nodes (§19); no ?role= override — a member can't read the
// treasurer's seat view by asking (Rule 24).
func (h *AnalyticsHandler) Dashboard(w http.ResponseWriter, r *http.Request) {
	raw, err := h.svc.Dashboard(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		analyticsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, raw)
}

func (h *AnalyticsHandler) Health(w http.ResponseWriter, r *http.Request) {
	row, err := h.svc.Health(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		analyticsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, row)
}

func (h *AnalyticsHandler) ListReports(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListRuns(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		analyticsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, rows)
}

type reportReq struct {
	Kind   string `json:"kind"` // monthly|annual|custom|handover
	Period string `json:"period"`
}

func (h *AnalyticsHandler) RunReport(w http.ResponseWriter, r *http.Request) {
	var req reportReq
	if err := httpx.Decode(w, r, &req); err != nil || req.Kind == "" {
		httpx.WriteErr(w, 400, "validation_failed", "kind required")
		return
	}
	id, err := h.svc.RunReport(r.Context(), middleware.OrgCtx(r.Context()), req.Kind, req.Period, nil)
	if err != nil {
		analyticsErr(w, err)
		return
	}
	httpx.WriteData(w, 201, map[string]any{"report_id": id})
}

// DownloadReport — returns the run row; the document_id links to the
// documents download route for the actual artifact.
func (h *AnalyticsHandler) DownloadReport(w http.ResponseWriter, r *http.Request) {
	rid, err := uuid.Parse(chi.URLParam(r, "rid"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	rc, name, err := h.svc.DownloadReport(r.Context(), middleware.OrgCtx(r.Context()), rid)
	if err != nil {
		analyticsErr(w, err)
		return
	}
	defer rc.Close()
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = io.Copy(w, rc)
}

func (h *AnalyticsHandler) Consolidated(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Consolidated(r.Context(), middleware.OrgCtx(r.Context()))
	if err != nil {
		analyticsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}

// Me — /me/analytics.
func (h *AnalyticsHandler) Me(w http.ResponseWriter, r *http.Request) {
	out, err := h.svc.Me(r.Context(), middleware.UserID(r.Context()))
	if err != nil {
		analyticsErr(w, err)
		return
	}
	httpx.WriteData(w, 200, out)
}
