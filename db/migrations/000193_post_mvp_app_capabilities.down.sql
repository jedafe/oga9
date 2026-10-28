DELETE FROM plan_capabilities WHERE (plan_slug, capability) IN (
  ('professional','app.projects'),('professional','app.assets'),('professional','app.integrations'),('professional','app.voice_rooms'),
  ('business','app.projects'),('business','app.assets'),('business','app.integrations'),('business','app.voice_rooms'),
  ('enterprise','app.projects'),('enterprise','app.assets'),('enterprise','app.integrations'),('enterprise','app.voice_rooms')
);
