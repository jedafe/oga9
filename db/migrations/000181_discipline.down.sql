UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  ((SELECT jsonb_agg(x) FROM jsonb_array_elements(manifest::jsonb->'permissions') x
    WHERE x::text NOT LIKE '"org.discipline.%')))::jsonb
WHERE app_id = 'governance';
DELETE FROM role_permissions WHERE permission LIKE 'org.discipline.%';
DELETE FROM permissions WHERE node LIKE 'org.discipline.%';
DROP TABLE IF EXISTS discipline_appeals;
DROP TABLE IF EXISTS discipline_hearings;
DROP TABLE IF EXISTS discipline_cases;
