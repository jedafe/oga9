-- Ref: oga9-engineering/sections/17-welfare.md

CREATE TABLE welfare_cases (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  kind text NOT NULL,
  title text NOT NULL, details text,
  requested_amount_minor bigint, currency text,
  auto_suggested boolean NOT NULL DEFAULT false,   -- MemberDeceased → bereavement suggestion
  state text NOT NULL DEFAULT 'requested',          -- requested|reviewing|approved|rejected|disbursed|closed
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX welfare_cases_org ON welfare_cases (organization_id, created_at DESC);
CREATE INDEX welfare_cases_member ON welfare_cases (membership_id, kind, created_at DESC);

CREATE TABLE welfare_case_events (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id uuid NOT NULL REFERENCES welfare_cases(id) ON DELETE CASCADE,
  actor_id uuid REFERENCES organization_memberships(id),
  action text NOT NULL, note text,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX welfare_case_events_case ON welfare_case_events (case_id, created_at);

CREATE TABLE welfare_approvals (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id uuid NOT NULL REFERENCES welfare_cases(id) ON DELETE CASCADE,
  approver_id uuid NOT NULL REFERENCES organization_memberships(id),
  decision text NOT NULL,                          -- approved|rejected
  approved_amount_minor bigint, currency text,
  decided_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX welfare_approvals_case ON welfare_approvals (case_id);

CREATE TABLE welfare_visits (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id uuid NOT NULL REFERENCES welfare_cases(id) ON DELETE CASCADE,
  scheduled_at timestamptz, completed_at timestamptz,
  report text
);
CREATE INDEX welfare_visits_case ON welfare_visits (case_id);

CREATE TABLE welfare_visit_delegates (
  visit_id uuid NOT NULL REFERENCES welfare_visits(id) ON DELETE CASCADE,
  member_id uuid NOT NULL REFERENCES organization_memberships(id),
  PRIMARY KEY (visit_id, member_id)
);

CREATE TABLE welfare_benefits (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  case_id uuid NOT NULL REFERENCES welfare_cases(id) ON DELETE CASCADE,
  kind text NOT NULL,                              -- cash|item|service|visit
  amount_minor bigint, currency text,
  payment_id uuid,
  disbursed_at timestamptz
);
CREATE INDEX welfare_benefits_case ON welfare_benefits (case_id);

-- 'welfare' app: requires organization_core + members + finance_lite;
-- app.welfare entitlement gates activation (basic+).
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('welfare','1.0.0','Welfare',false,'{
  "id":"welfare","version":"1.0.0","requires":["organization_core","members","finance_lite"],
  "permissions":["org.welfare.request_own","org.welfare.view_any","org.welfare.review","org.welfare.approve","org.welfare.disburse","org.welfare.visit"],
  "events":{"publishes":["WelfareRequested","WelfareReviewed","SupportApproved","SupportRejected","BenefitDisbursed","VisitScheduled","VisitCompleted","CaseClosed","CondolenceRegistered"],"consumes":["MemberDeceased"]},
  "entitlements":{"feature":"app.welfare"}
}');
