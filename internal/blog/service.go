// Ref: oga9-engineering/sections/28-platform-blog.md
// Platform blog — staff-authored marketing/editorial content. Platform-scoped:
// no org context, no tenant column; public readers hit the published routes,
// writers hold platform.blog.manage (control-plane surface).
package blog

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/jedafe/oga9/internal/db"
)

var (
	ErrNotFound          = errors.New("not_found")
	ErrBadValue          = errors.New("invalid_value")
	ErrSlugTaken         = errors.New("slug_taken")
	ErrInvalidTransition = errors.New("invalid_state_transition")
)

var validSlugRe = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type Service struct {
	pool *pgxpool.Pool
	q    *db.Queries
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool, q: db.New(pool)}
}

func toPg(u uuid.UUID) pgtype.UUID   { return pgtype.UUID{Bytes: u, Valid: u != uuid.Nil} }
func pgText(s string) pgtype.Text    { return pgtype.Text{String: s, Valid: s != ""} }
func fromPg(p pgtype.UUID) uuid.UUID { u, _ := uuid.FromBytes(p.Bytes[:]); return u }

// ---------- public reads ----------

func (s *Service) ListPublished(ctx context.Context, tag string, lim, off int32) ([]db.ListBlogPostsPublishedRow, int64, error) {
	rows, err := s.q.ListBlogPostsPublished(ctx, db.ListBlogPostsPublishedParams{Tag: tag, Lim: lim, Off: off})
	if err != nil {
		return nil, 0, err
	}
	total, err := s.q.CountBlogPostsPublished(ctx, tag)
	return rows, total, err
}

func (s *Service) GetPublished(ctx context.Context, slug string) (*db.BlogPosts, error) {
	p, err := s.q.GetBlogPostBySlug(ctx, slug)
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ---------- admin (platform.blog.manage) ----------

func (s *Service) ListAll(ctx context.Context, state string, lim, off int32) ([]db.BlogPosts, int64, error) {
	rows, err := s.q.ListBlogPostsAdmin(ctx, db.ListBlogPostsAdminParams{State: state, Lim: lim, Off: off})
	if err != nil {
		return nil, 0, err
	}
	total, err := s.q.CountBlogPostsAdmin(ctx, state)
	return rows, total, err
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*db.BlogPosts, error) {
	p, err := s.q.GetBlogPost(ctx, toPg(id))
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *Service) Create(ctx context.Context, slug, title, excerpt, bodyMd string, tags []string, authorID uuid.UUID, authorName string) (*db.BlogPosts, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if !validSlugRe.MatchString(slug) || title == "" || bodyMd == "" {
		return nil, ErrBadValue
	}
	if tags == nil {
		tags = []string{} // column is NOT NULL — nil binds NULL and violates it
	}
	p, err := s.q.CreateBlogPost(ctx, db.CreateBlogPostParams{
		Slug: slug, Title: title, Excerpt: pgText(excerpt), BodyMd: bodyMd,
		Tags: tags, AuthorID: toPg(authorID), AuthorName: pgText(authorName),
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	return &p, nil
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, slug, title, excerpt, bodyMd string, tags []string, authorName string) (*db.BlogPosts, error) {
	if title == "" || bodyMd == "" {
		return nil, ErrBadValue
	}
	slug = strings.ToLower(strings.TrimSpace(slug))
	if slug != "" && !validSlugRe.MatchString(slug) {
		return nil, ErrBadValue
	}
	if tags == nil {
		tags = []string{}
	}
	p, err := s.q.UpdateBlogPost(ctx, db.UpdateBlogPostParams{
		ID: toPg(id), Slug: pgText(slug), Title: title, Excerpt: pgText(excerpt),
		BodyMd: bodyMd, Tags: tags, AuthorName: pgText(authorName),
	})
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		if isUniqueViolation(err) {
			return nil, ErrSlugTaken
		}
		return nil, err
	}
	return &p, nil
}

// Transition — draft→published (stamps published_at once) →unpublish→draft;
// delete archives (soft — published history is never hard-dropped).
func (s *Service) Transition(ctx context.Context, id uuid.UUID, to string) (*db.BlogPosts, error) {
	if to != "draft" && to != "published" && to != "archived" {
		return nil, ErrInvalidTransition
	}
	p, err := s.q.SetBlogPostStatus(ctx, db.SetBlogPostStatusParams{ID: toPg(id), Status: to})
	if err == pgx.ErrNoRows {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
