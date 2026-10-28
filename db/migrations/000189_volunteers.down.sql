UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  ((manifest::jsonb->'permissions')::jsonb -
    '["org.volunteers.view","org.volunteers.signup_own","org.volunteers.manage"]'::jsonb))::json
WHERE app_id = 'events_meetings';
DELETE FROM role_permissions WHERE permission IN (
  'org.volunteers.view','org.volunteers.signup_own','org.volunteers.manage');
DELETE FROM permissions WHERE node IN (
  'org.volunteers.view','org.volunteers.signup_own','org.volunteers.manage');
DROP TABLE volunteer_signups;
DROP TABLE volunteer_roles;
