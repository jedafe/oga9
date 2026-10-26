-- Ref: oga9-engineering/sections/12-finance-lite.md, Rule 9 — payment records.
-- claim_id FK lands in 000104 (payment_claims created after); activity_id and
-- receipt_media_id stay untyped-FK until groups 10/11 land (MIGRATION-ORDER).
CREATE TABLE payments (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  obligation_id uuid REFERENCES obligations(id),
  amount_minor bigint NOT NULL, currency text NOT NULL,
  channel text NOT NULL,                       -- MVP: cash|transfer|pos|other_manual
  status text NOT NULL DEFAULT 'initiated',    -- initiated|confirmed|reconciled|failed|reversed
  provider_ref text, external_ref text,
  benefit_member_id uuid REFERENCES organization_memberships(id),
  activity_id uuid,                            -- -> activities(id): FK added in g10
  idempotency_key text UNIQUE,                 -- Rule 9
  claim_id uuid,                               -- -> payment_claims(id): FK in 000104
  drive_id uuid REFERENCES contribution_drives(id),
  recorded_by uuid REFERENCES organization_memberships(id),
  approved_by uuid REFERENCES organization_memberships(id),
  confirmed_at timestamptz, created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payments_org_status ON payments (organization_id, status);
CREATE INDEX payments_obligation ON payments (obligation_id)
  WHERE status IN ('confirmed','reconciled');
CREATE INDEX payments_drive ON payments (drive_id);
CREATE INDEX payments_idem ON payments (organization_id, idempotency_key)
  WHERE idempotency_key IS NOT NULL;
