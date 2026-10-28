-- Ref: oga9-engineering/conventions/plans-entitlements-catalog.md
-- Post-MVP app feature keys (app.projects, app.assets, app.integrations,
-- app.voice_rooms) were checked at activation but granted by no plan or
-- addon — un-activatable on cloud. Catalogued as professional+ bundle
-- (same tier as governance/elections); entitlement resolution is
-- plan ∪ addons ∪ license with no lower-tier inheritance, so each tier
-- gets the explicit set.
INSERT INTO plan_capabilities (plan_slug, capability) VALUES
  ('professional','app.projects'),('professional','app.assets'),('professional','app.integrations'),('professional','app.voice_rooms'),
  ('business','app.projects'),('business','app.assets'),('business','app.integrations'),('business','app.voice_rooms'),
  ('enterprise','app.projects'),('enterprise','app.assets'),('enterprise','app.integrations'),('enterprise','app.voice_rooms')
ON CONFLICT (plan_slug, capability) DO NOTHING;
