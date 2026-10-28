UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  ((manifest::jsonb->'permissions')::jsonb -
    '["org.comms.polls.manage","org.comms.polls.vote_own","org.comms.surveys.manage","org.comms.surveys.respond_own"]'::jsonb))::json
WHERE app_id = 'comms_documents';
DELETE FROM role_permissions WHERE permission IN (
  'org.comms.polls.manage','org.comms.polls.vote_own',
  'org.comms.surveys.manage','org.comms.surveys.respond_own');
DELETE FROM permissions WHERE node IN (
  'org.comms.polls.manage','org.comms.polls.vote_own',
  'org.comms.surveys.manage','org.comms.surveys.respond_own');
DROP TABLE survey_responses;
DROP TABLE surveys;
DROP TABLE poll_votes;
DROP TABLE poll_ballots;
DROP TABLE poll_options;
DROP TABLE polls;
