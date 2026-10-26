-- Ref: oga9-engineering/sections/12-finance-lite.md, Rule 19 — append-only
-- ledger (corrections via reversal_of), receipts, reconciliation records.
-- receipt_no is an ORG-scoped sequence (RCP-YYYY-NNNNN) — uniqueness is
-- per-org: two orgs may each issue RCP-2026-00001.
ALTER TABLE organizations ADD COLUMN receipt_no_seq int NOT NULL DEFAULT 0;

CREATE TABLE ledger_entries (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  payment_id uuid REFERENCES payments(id),
  reversal_of uuid REFERENCES ledger_entries(id),
  direction text NOT NULL,                      -- credit|debit
  account text NOT NULL,                        -- 'members_dues','levy','welfare','projects','general'
  amount_minor bigint NOT NULL, currency text NOT NULL,
  memo text,
  posted_by uuid REFERENCES organization_memberships(id),
  posted_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ledger_entries_org ON ledger_entries (organization_id, posted_at);
CREATE INDEX ledger_entries_payment ON ledger_entries (payment_id);

CREATE TABLE receipts (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  payment_id uuid NOT NULL REFERENCES payments(id),
  receipt_no text NOT NULL,                     -- 'RCP-2026-00001' — unique per org
  issued_to uuid NOT NULL REFERENCES organization_memberships(id),
  issued_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE (organization_id, receipt_no)
);
CREATE UNIQUE INDEX receipts_payment ON receipts (payment_id);

CREATE TABLE reconciliation_records (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  payment_id uuid NOT NULL REFERENCES payments(id),
  reconciled_by uuid NOT NULL REFERENCES organization_memberships(id),
  note text, reconciled_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX reconciliation_payment ON reconciliation_records (payment_id);
