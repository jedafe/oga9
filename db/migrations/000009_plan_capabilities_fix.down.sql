DELETE FROM plan_capabilities WHERE (plan_slug, capability) IN (
  ('basic','app.organization_core'),('basic','app.members'),('basic','app.events_meetings'),('basic','app.comms_documents'),
  ('professional','app.organization_core'),('professional','app.members'),('professional','app.events_meetings'),('professional','app.comms_documents'),
  ('business','app.organization_core'),('business','app.members'),('business','app.events_meetings'),('business','app.comms_documents'),
  ('enterprise','app.organization_core'),('enterprise','app.members'),('enterprise','app.events_meetings'),('enterprise','app.comms_documents')
);
