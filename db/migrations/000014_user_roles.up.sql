CREATE TABLE user_roles (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid REFERENCES tenants(id),
  user_group_id uuid REFERENCES user_groups(id) ON DELETE CASCADE,
  name text NOT NULL, slug text NOT NULL,
  is_system boolean NOT NULL DEFAULT false,
  UNIQUE(tenant_id, slug)
);
