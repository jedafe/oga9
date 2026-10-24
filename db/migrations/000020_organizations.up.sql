-- Ref: oga9-engineering/sections/04-tenancy-organizations.md
CREATE TABLE organizations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  parent_org_id uuid REFERENCES organizations(id),
  name text NOT NULL, slug text NOT NULL,
  template_slug text,
  org_type text NOT NULL,
  country_code text, city text,
  is_public_listed boolean NOT NULL DEFAULT false,
  maturity_level int NOT NULL DEFAULT 1,
  created_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz,
  UNIQUE(tenant_id, slug)
);
-- circular-FK resolution: tenants.owner_org_id stub → backref (MIGRATION-ORDER)
ALTER TABLE tenants ADD CONSTRAINT tenants_owner_org_fk
  FOREIGN KEY (owner_org_id) REFERENCES organizations(id);
CREATE INDEX organizations_tenant ON organizations (tenant_id) WHERE deleted_at IS NULL;
