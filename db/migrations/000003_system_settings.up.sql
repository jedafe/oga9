-- Ref: oga9-engineering/sections/04-tenancy-organizations.md
CREATE TABLE system_settings (
  key text PRIMARY KEY, value jsonb NOT NULL,
  updated_at timestamptz NOT NULL DEFAULT now()
);
