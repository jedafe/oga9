-- Ref: oga9-engineering/sections/06-app-framework-registry.md
CREATE TABLE app_registry (                    -- OGA9-maintained definitions
  app_id text PRIMARY KEY,                     -- 'members','finance_lite',...
  version text NOT NULL, label text NOT NULL,
  manifest jsonb NOT NULL,                     -- requires/permissions/events/entitlements
  mvp_required boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now()
);
