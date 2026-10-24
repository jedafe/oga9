-- Ref: oga9-engineering/sections/10-organization-core.md (group 8)
CREATE TABLE org_branding (
  organization_id uuid PRIMARY KEY REFERENCES organizations(id),
  logo_media_id uuid,
  primary_color text, secondary_color text,
  motto text, founded_year int,
  portal_config jsonb NOT NULL DEFAULT '{}'
);
