-- restore the partial §13 node surface.
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.activities.view","org.events.manage","org.events.register_own","org.minutes.approve"]')
WHERE app_id = 'events_meetings';
