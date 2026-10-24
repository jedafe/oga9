CREATE TABLE permissions (
  node text PRIMARY KEY,
  label text NOT NULL, description text,
  created_at timestamptz NOT NULL DEFAULT now()
);
