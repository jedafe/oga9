// Ref: oga9-engineering/sections/28-platform-blog.md
// Platform blog — public published reads + platform.blog.manage admin CRUD.
package handler

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/jedafe/oga9/internal/blog"
	"github.com/jedafe/oga9/internal/httpx"
	"github.com/jedafe/oga9/internal/middleware"
)

type BlogHandler struct {
	svc     *blog.Service
	baseURL string
}

func NewBlogHandler(svc *blog.Service, baseURL string) *BlogHandler {
	return &BlogHandler{svc: svc, baseURL: strings.TrimSuffix(baseURL, "/")}
}

func blogErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, blog.ErrNotFound):
		httpx.WriteErr(w, 404, "not_found", "post not found")
	case errors.Is(err, blog.ErrBadValue):
		httpx.WriteErr(w, 400, "invalid_value", "invalid field value")
	case errors.Is(err, blog.ErrSlugTaken):
		httpx.WriteErr(w, 409, "slug_taken", "slug already in use")
	case errors.Is(err, blog.ErrInvalidTransition):
		httpx.WriteErr(w, 409, "invalid_state_transition", "status transition not allowed")
	default:
		httpx.WriteErr(w, 500, "internal_error", "blog operation failed")
	}
}

// ---------- public ----------

func (h *BlogHandler) Posts(w http.ResponseWriter, r *http.Request) {
	page, per := httpx.PageParams(r)
	rows, total, err := h.svc.ListPublished(r.Context(),
		r.URL.Query().Get("tag"), int32(per), int32((page-1)*per))
	if err != nil {
		blogErr(w, err)
		return
	}
	httpx.WritePage(w, 200, rows, page, per, total)
}

func (h *BlogHandler) Post(w http.ResponseWriter, r *http.Request) {
	p, err := h.svc.GetPublished(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		blogErr(w, err)
		return
	}
	httpx.WriteData(w, 200, p)
}

// RSS — RSS 2.0 of published posts, capped at 50.
func (h *BlogHandler) RSS(w http.ResponseWriter, r *http.Request) {
	rows, _, err := h.svc.ListPublished(r.Context(), "", 50, 0)
	if err != nil {
		blogErr(w, err)
		return
	}
	type item struct {
		Title   string `xml:"title"`
		Link    string `xml:"link"`
		Desc    string `xml:"description"`
		PubDate string `xml:"pubDate"`
		GUID    string `xml:"guid"`
	}
	items := make([]item, 0, len(rows))
	for _, p := range rows {
		link := h.baseURL + "/blog/" + p.Slug
		it := item{Title: p.Title, Link: link, Desc: p.Excerpt.String, GUID: link}
		if p.PublishedAt.Valid {
			it.PubDate = p.PublishedAt.Time.Format(time.RFC1123Z)
		}
		items = append(items, it)
	}
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><rss version="2.0"><channel><title>OGA9 Blog</title><link>%s/blog</link><description>OGA9 platform blog</description>`, h.baseURL)
	enc := xml.NewEncoder(w)
	for _, it := range items {
		_ = enc.Encode(it)
	}
	fmt.Fprint(w, `</channel></rss>`)
}

// ---------- admin (platform.blog.manage) ----------

func (h *BlogHandler) AdminList(w http.ResponseWriter, r *http.Request) {
	page, per := httpx.PageParams(r)
	rows, total, err := h.svc.ListAll(r.Context(),
		r.URL.Query().Get("state"), int32(per), int32((page-1)*per))
	if err != nil {
		blogErr(w, err)
		return
	}
	httpx.WritePage(w, 200, rows, page, per, total)
}

func (h *BlogHandler) AdminGet(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "post_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	p, err := h.svc.Get(r.Context(), id)
	if err != nil {
		blogErr(w, err)
		return
	}
	httpx.WriteData(w, 200, p)
}

type postReq struct {
	Slug       string   `json:"slug"`
	Title      string   `json:"title"`
	Excerpt    string   `json:"excerpt"`
	Body       string   `json:"body"`
	Tags       []string `json:"tags"`
	AuthorName string   `json:"author_name"`
}

func (h *BlogHandler) AdminCreate(w http.ResponseWriter, r *http.Request) {
	var req postReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	p, err := h.svc.Create(r.Context(), req.Slug, req.Title, req.Excerpt, req.Body,
		req.Tags, middleware.UserID(r.Context()), req.AuthorName)
	if err != nil {
		blogErr(w, err)
		return
	}
	httpx.WriteData(w, 201, p)
}

func (h *BlogHandler) AdminPatch(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "post_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	var req postReq
	if err := httpx.Decode(w, r, &req); err != nil {
		httpx.WriteErr(w, 400, "validation_failed", "invalid body")
		return
	}
	p, err := h.svc.Update(r.Context(), id, req.Slug, req.Title, req.Excerpt, req.Body,
		req.Tags, req.AuthorName)
	if err != nil {
		blogErr(w, err)
		return
	}
	httpx.WriteData(w, 200, p)
}

func (h *BlogHandler) AdminPublish(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, "published")
}
func (h *BlogHandler) AdminUnpublish(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, "draft")
}
func (h *BlogHandler) AdminDelete(w http.ResponseWriter, r *http.Request) {
	h.transition(w, r, "archived")
}

func (h *BlogHandler) transition(w http.ResponseWriter, r *http.Request, to string) {
	id, err := uuid.Parse(chi.URLParam(r, "post_id"))
	if err != nil {
		httpx.WriteErr(w, 400, "invalid_id", "malformed identifier")
		return
	}
	p, err := h.svc.Transition(r.Context(), id, to)
	if err != nil {
		blogErr(w, err)
		return
	}
	httpx.WriteData(w, 200, p)
}
