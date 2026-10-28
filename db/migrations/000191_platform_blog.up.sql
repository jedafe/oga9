-- Ref: oga9-engineering/sections/28-platform-blog.md
CREATE TABLE blog_posts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug text NOT NULL UNIQUE,
  title text NOT NULL,
  excerpt text,
  body_md text NOT NULL,
  status text NOT NULL DEFAULT 'draft',        -- draft|published|archived
  tags text[] NOT NULL DEFAULT '{}',
  author_id uuid REFERENCES users(id),
  author_name text,
  published_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now(),
  updated_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX blog_posts_published ON blog_posts (published_at DESC) WHERE status = 'published';
CREATE INDEX blog_posts_tags ON blog_posts USING gin (tags) WHERE status = 'published';

INSERT INTO permissions (node, label) VALUES
  ('platform.blog.manage','Manage platform blog')
ON CONFLICT (node) DO NOTHING;
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, 'platform.blog.manage' FROM user_roles r
WHERE r.slug = 'platform_admin' AND r.tenant_id IS NULL
ON CONFLICT DO NOTHING;
