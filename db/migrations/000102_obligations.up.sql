-- Ref: oga9-engineering/sections/12-finance-lite.md — what a member owes.
-- Idempotent per (membership, type, period_label) — generators replay safely.
CREATE TABLE obligations (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  contribution_type_id uuid NOT NULL REFERENCES contribution_types(id),
  schedule_id uuid REFERENCES dues_schedules(id),
  period_label text,                           -- '2026-11' / 'registration'
  amount_minor bigint NOT NULL, currency text NOT NULL,
  status text NOT NULL DEFAULT 'open',         -- open|partially_paid|paid|waived|overdue|cancelled
  due_date date NOT NULL,
  waived_by uuid REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(membership_id, contribution_type_id, period_label)
);
CREATE INDEX obligations_org_status ON obligations (organization_id, status);
CREATE INDEX obligations_due ON obligations (due_date)
  WHERE status IN ('open','partially_paid');
