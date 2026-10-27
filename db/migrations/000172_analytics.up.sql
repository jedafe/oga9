-- Ref: oga9-engineering/sections/19-analytics.md — digital-twin read side:
-- async snapshots (dashboards never scan domain tables per-request),
-- versioned health scores, report runs, KPI definitions.

CREATE TABLE health_scores (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  period text NOT NULL,
  score int NOT NULL,
  components jsonb NOT NULL,
  formula_version text NOT NULL DEFAULT 'v1',
  computed_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(organization_id, period)
);

CREATE TABLE report_runs (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  kind text NOT NULL,                            -- monthly|annual|custom|handover
  period text, params jsonb,
  document_id uuid,
  generated_by uuid REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX report_runs_org ON report_runs (organization_id, created_at DESC);

CREATE TABLE dashboard_snapshots (
  organization_id uuid NOT NULL REFERENCES organizations(id),
  role text NOT NULL,                            -- president|treasurer|secretary|member
  snapshot jsonb NOT NULL,
  refreshed_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (organization_id, role)
);

CREATE TABLE kpi_definitions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id),  -- null = global default
  key text NOT NULL, formula jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now()
);

-- 'analytics' app: business-tier digital twin (requires finance_lite +
-- events_meetings); app.analytics entitlement gates activation.
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('analytics','1.0.0','Analytics',false,'{
  "id":"analytics","version":"1.0.0","requires":["organization_core","members","finance_lite","events_meetings"],
  "permissions":["org.analytics.view","org.analytics.reports.generate","org.analytics.export","org.analytics.consolidated"],
  "events":{"publishes":["HealthScoreCalculated","MonthlyReportGenerated","AnnualReportGenerated","KPIThresholdCrossed","DashboardSnapshotRefreshed"],"consumes":["MemberApproved","AttendanceRecorded","PaymentReceived","InvoiceIssued","EventCompleted","MinutesApproved","WelfareRequested","CaseClosed"]},
  "entitlements":{"feature":"app.analytics"}
}');
