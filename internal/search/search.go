// Ref: oga9-engineering/sections/14-comms-documents.md + ADR-019 —
// Meilisearch indexing consumer ("search" is the catalog-reserved consumer
// name for DocumentUploaded/DocumentVersioned/DocumentAccessChanged).
// The engine is phased: unconfigured → consumer acks and does nothing
// (self-hosted keeps working without it, Rule 29; PG FTS stays the floor).
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/jedafe/oga9/internal/db"
	"github.com/jedafe/oga9/internal/events"
)

const IndexDocuments = "oga9_documents"

type Service struct {
	url, key string
	hc       *http.Client
}

// New returns nil when Meilisearch is unconfigured — callers nil-check.
func New(baseURL, apiKey string) *Service {
	if baseURL == "" {
		return nil
	}
	return &Service{url: baseURL, key: apiKey, hc: &http.Client{Timeout: 10 * time.Second}}
}

func (s *Service) Enabled() bool { return s != nil }

func (s *Service) req(ctx context.Context, method, path string, body any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	r, err := http.NewRequestWithContext(ctx, method, s.url+path, rdr)
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	if s.key != "" {
		r.Header.Set("Authorization", "Bearer "+s.key)
	}
	resp, err := s.hc.Do(r)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("meilisearch %s %s: %d %s", method, path, resp.StatusCode, string(b))
	}
	return nil
}

// EnsureIndex is idempotent — called at startup when configured.
func (s *Service) EnsureIndex(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if err := s.req(ctx, "PUT", "/indexes/"+IndexDocuments+"/settings", map[string]any{
		"filterableAttributes": []string{"organization_id", "kind", "visibility"},
		"searchableAttributes": []string{"title"},
	}); err != nil {
		return err
	}
	return nil
}

func (s *Service) upsert(ctx context.Context, doc map[string]any) error {
	return s.req(ctx, "POST", "/indexes/"+IndexDocuments+"/documents?primaryKey=id", []map[string]any{doc})
}

func (s *Service) remove(ctx context.Context, id uuid.UUID) error {
	return s.req(ctx, "DELETE", "/indexes/"+IndexDocuments+"/documents/"+id.String(), nil)
}

// Consumer indexes live document rows; archived (soft-deleted) rows drop
// out of the index. Private docs stay out (§14 visibility).
func (s *Service) Consumer(ctx context.Context, q *db.Queries, e events.Event) error {
	if s == nil {
		return nil // unconfigured — phased service absent, nothing to index
	}
	if e.Type != "DocumentUploaded" && e.Type != "DocumentVersioned" && e.Type != "DocumentAccessChanged" {
		return nil
	}
	docID := e.UUID("document_id")
	if docID == uuid.Nil {
		docID = e.AggregateID
	}
	if docID == uuid.Nil {
		return nil
	}
	d, err := q.GetDocumentAny(ctx, pgtype.UUID{Bytes: docID, Valid: true})
	if err != nil || d.DeletedAt.Valid || d.Visibility == "private" {
		if err := s.remove(ctx, docID); err != nil {
			slog.Warn("search: unindex", "doc", docID, "err", err)
		}
		return nil
	}
	doc := map[string]any{
		"id":              docID.String(),
		"organization_id": uuid.UUID(d.OrganizationID.Bytes).String(),
		"title":           d.Title,
		"kind":            d.Kind,
		"visibility":      d.Visibility,
		"version":         d.Version,
	}
	if err := s.upsert(ctx, doc); err != nil {
		return err // retry — at-least-once keeps the index convergent
	}
	return nil
}
