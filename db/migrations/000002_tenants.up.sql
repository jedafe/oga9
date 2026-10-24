-- Ref: oga9-engineering/sections/04-tenancy-organizations.md
CREATE TABLE tenants (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  name text NOT NULL, slug text NOT NULL UNIQUE,
  billing_email citext NOT NULL,
  edition text NOT NULL DEFAULT 'cloud',
  status text NOT NULL DEFAULT 'trial',
  owner_org_id uuid,
  created_at timestamptz NOT NULL DEFAULT now()
);
