-- Ref: oga9-engineering/sections/26-polls-surveys.md (group 24) — polls are
-- consultative member voice (governance binding votes are §21 elections);
-- secrecy is structural: poll_ballots = turnout receipt, poll_votes =
-- content, no linkable FK when secret.

CREATE TABLE polls (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  created_by uuid NOT NULL REFERENCES organization_memberships(id),
  question text NOT NULL,
  kind text NOT NULL DEFAULT 'single',         -- single|multi
  secret boolean NOT NULL DEFAULT false,       -- resolved at open from org.comms.polls.secret_default
  audience jsonb NOT NULL DEFAULT '"all"',     -- same AudienceSpec as announcements (§14)
  state text NOT NULL DEFAULT 'draft',         -- draft|open|closed
  opens_at timestamptz, closes_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX polls_org ON polls (organization_id, state, created_at DESC);
CREATE INDEX polls_due ON polls (closes_at) WHERE state = 'open';

CREATE TABLE poll_options (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  poll_id uuid NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
  label text NOT NULL, position int NOT NULL DEFAULT 0
);
CREATE INDEX poll_options_poll ON poll_options (poll_id, position);

CREATE TABLE poll_ballots (                    -- receipt side: turnout, one per member
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  poll_id uuid NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  cast_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (poll_id, membership_id)
);

CREATE TABLE poll_votes (                      -- content side: no member link when secret
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  poll_id uuid NOT NULL REFERENCES polls(id) ON DELETE CASCADE,
  option_id uuid NOT NULL REFERENCES poll_options(id) ON DELETE CASCADE,
  membership_id uuid REFERENCES organization_memberships(id) -- set only when poll.secret=false
);
CREATE INDEX poll_votes_poll ON poll_votes (poll_id, option_id);

CREATE TABLE surveys (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  created_by uuid NOT NULL REFERENCES organization_memberships(id),
  title text NOT NULL, intro text,
  schema jsonb NOT NULL,                       -- SurveySchema [{key,label,type,required,options[]}]
  anonymous boolean NOT NULL DEFAULT false,    -- resolved at open from org.surveys.anonymous_default
  audience jsonb NOT NULL DEFAULT '"all"',
  state text NOT NULL DEFAULT 'draft',         -- draft|open|closed
  opens_at timestamptz, closes_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX surveys_org ON surveys (organization_id, state, created_at DESC);
CREATE INDEX surveys_due ON surveys (closes_at) WHERE state = 'open';

CREATE TABLE survey_responses (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  survey_id uuid NOT NULL REFERENCES surveys(id) ON DELETE CASCADE,
  membership_id uuid REFERENCES organization_memberships(id), -- NULL when survey.anonymous
  answers jsonb NOT NULL,
  submitted_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (survey_id, membership_id)            -- one response per member (NULLs not equal — anonymous dedupes via the submit-token marker)
);
CREATE INDEX survey_responses_survey ON survey_responses (survey_id);

-- §26 permission nodes; scoped re-grants let migrated DBs catch up.
INSERT INTO permissions (node, label) VALUES
('org.comms.polls.manage','Manage polls'),
('org.comms.polls.vote_own','Vote in polls'),
('org.comms.surveys.manage','Manage surveys'),
('org.comms.surveys.respond_own','Respond to surveys');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'org_admin' AND r.tenant_id IS NULL AND p.node IN (
  'org.comms.polls.manage','org.comms.polls.vote_own',
  'org.comms.surveys.manage','org.comms.surveys.respond_own');
-- catalog: manage → PRO/secretary; vote_own/respond_own → members.
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug IN ('pro','secretary') AND r.tenant_id IS NULL
  AND p.node IN ('org.comms.polls.manage','org.comms.surveys.manage');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'member' AND r.tenant_id IS NULL
  AND p.node IN ('org.comms.polls.vote_own','org.comms.surveys.respond_own');

UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{permissions}',
  (manifest::jsonb->'permissions') ||
  '["org.comms.polls.manage","org.comms.polls.vote_own","org.comms.surveys.manage","org.comms.surveys.respond_own"]'::jsonb)::json
WHERE app_id = 'comms_documents';
