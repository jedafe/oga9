-- Ref: oga9-engineering/sections/28-platform-blog.md — platform blog queries.

-- name: CreateBlogPost :one
INSERT INTO blog_posts (slug, title, excerpt, body_md, tags, author_id, author_name)
VALUES (sqlc.arg(slug), sqlc.arg(title), sqlc.narg(excerpt)::text, sqlc.arg(body_md),
  sqlc.arg(tags)::text[], sqlc.narg(author_id)::uuid, sqlc.narg(author_name)::text)
RETURNING *;

-- name: GetBlogPost :one
SELECT * FROM blog_posts WHERE id = sqlc.arg(id)::uuid;

-- name: GetBlogPostBySlug :one
SELECT * FROM blog_posts WHERE slug = sqlc.arg(slug) AND status = 'published';

-- name: ListBlogPostsPublished :many
-- Listing rows stay slim — body_md belongs to the detail read, not the index.
SELECT id, slug, title, excerpt, tags, author_name, published_at
FROM blog_posts WHERE status = 'published'
  AND (sqlc.arg(tag)::text = '' OR sqlc.arg(tag)::text = ANY(tags))
ORDER BY published_at DESC LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountBlogPostsPublished :one
SELECT count(*) FROM blog_posts WHERE status = 'published'
  AND (sqlc.arg(tag)::text = '' OR sqlc.arg(tag)::text = ANY(tags));

-- name: ListBlogPostsAdmin :many
SELECT * FROM blog_posts WHERE (sqlc.arg(state)::text = '' OR status = sqlc.arg(state))
ORDER BY updated_at DESC LIMIT sqlc.arg(lim) OFFSET sqlc.arg(off);

-- name: CountBlogPostsAdmin :one
SELECT count(*) FROM blog_posts WHERE (sqlc.arg(state)::text = '' OR status = sqlc.arg(state));

-- name: UpdateBlogPost :one
UPDATE blog_posts SET title = sqlc.arg(title), excerpt = sqlc.narg(excerpt)::text,
  body_md = sqlc.arg(body_md), tags = sqlc.arg(tags)::text[],
  author_name = sqlc.narg(author_name)::text, updated_at = now(),
  slug = CASE WHEN status = 'draft' THEN COALESCE(sqlc.narg(slug)::text, slug) ELSE slug END
WHERE id = sqlc.arg(id) RETURNING *;

-- name: SetBlogPostStatus :one
UPDATE blog_posts SET status = sqlc.arg(status), updated_at = now(),
  published_at = CASE WHEN sqlc.arg(status)::text = 'published' AND published_at IS NULL THEN now() ELSE published_at END
WHERE id = sqlc.arg(id)::uuid RETURNING *;
