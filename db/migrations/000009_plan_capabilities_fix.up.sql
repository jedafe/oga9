-- Ref: oga9-engineering/conventions/plans-entitlements-catalog.md
-- 000008 under-seeded: cumulative capabilities (all-plans + starter+) were
-- only granted to 'starter'. Entitlement resolution is plan ∪ addons ∪
-- license (§07) with no lower-tier inheritance, so every paid plan needs the
-- full cumulative set explicitly.
INSERT INTO plan_capabilities (plan_slug, capability) VALUES
  ('basic','app.organization_core'),('basic','app.members'),('basic','app.events_meetings'),('basic','app.comms_documents'),
  ('professional','app.organization_core'),('professional','app.members'),('professional','app.events_meetings'),('professional','app.comms_documents'),
  ('business','app.organization_core'),('business','app.members'),('business','app.events_meetings'),('business','app.comms_documents'),
  ('enterprise','app.organization_core'),('enterprise','app.members'),('enterprise','app.events_meetings'),('enterprise','app.comms_documents')
ON CONFLICT (plan_slug, capability) DO NOTHING;
