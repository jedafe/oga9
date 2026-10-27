-- Ref: oga9-engineering/sections/21-elections.md — secrecy is STRUCTURAL:
-- ballots prove participation (unique per member); votes carry no
-- member/ballot link — no FK, no correlatable field.

CREATE TABLE elections (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  term_id uuid NOT NULL REFERENCES terms(id),      -- elects NEXT term
  title text NOT NULL, state text NOT NULL DEFAULT 'setup', -- setup|nominations|campaign|voting|concluded|ratified|annulled
  nominations_open timestamptz, voting_opens timestamptz, voting_closes timestamptz,
  eligibility jsonb NOT NULL DEFAULT '{}',          -- category/tenure/standing rules
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX elections_org ON elections (organization_id, state);

CREATE TABLE candidates (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  election_id uuid NOT NULL REFERENCES elections(id),
  position_id uuid NOT NULL REFERENCES org_positions(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  manifesto text, state text NOT NULL DEFAULT 'nominated', -- nominated|vetted|withdrawn|disqualified
  nominated_by uuid REFERENCES organization_memberships(id),
  UNIQUE(election_id, position_id, membership_id)
);
CREATE INDEX candidates_election ON candidates (election_id, position_id);

CREATE TABLE ballots (                            -- participation receipt ONLY
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  election_id uuid NOT NULL REFERENCES elections(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  cast_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(election_id, membership_id)                -- one vote per member
);

CREATE TABLE votes (                              -- ANONYMOUS — no FK to ballots
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  election_id uuid NOT NULL REFERENCES elections(id),
  position_id uuid NOT NULL REFERENCES org_positions(id),
  candidate_id uuid REFERENCES candidates(id),     -- null = abstain that seat
  cast_seq bigint NOT NULL                         -- ordering nonce only
);
CREATE INDEX votes_tally ON votes (election_id, position_id);

-- 'elections' app: requires organization_core + members + governance;
-- app.elections capability gates activation (professional+).
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('elections','1.0.0','Elections',false,'{
  "id":"elections","version":"1.0.0","requires":["organization_core","members","governance"],
  "permissions":["org.elections.manage","org.elections.nominate","org.elections.vet","org.elections.vote_own","org.elections.results.view","org.elections.ratify","org.elections.audit.view"],
  "events":{"publishes":["ElectionStarted","NominationsOpened","CandidateNominated","CandidateVetted","VotingOpened","BallotCast","VotingClosed","ElectionConcluded","ElectionRatified","ElectionAnnulled"],"consumes":[]},
  "entitlements":{"feature":"app.elections"}
}');
