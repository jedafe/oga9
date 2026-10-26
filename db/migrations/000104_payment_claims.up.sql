-- Ref: oga9-engineering/sections/12-finance-lite.md — member-reported
-- remittance → officer verification. Universal provenance: every member
-- payment is claim→confirm or record→approve.
CREATE TABLE payment_claims (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id), -- who paid
  drive_id uuid REFERENCES contribution_drives(id),
  obligation_id uuid REFERENCES obligations(id),
  amount_minor bigint NOT NULL, currency text NOT NULL,
  channel text NOT NULL DEFAULT 'transfer',     -- transfer|cash|pos|other_manual
  external_ref text,                            -- bank tx/ussd reference
  receipt_media_id uuid,                        -- -> documents: FK in g11
  note text,
  state text NOT NULL DEFAULT 'submitted',       -- submitted|confirmed|rejected
  reviewed_by uuid REFERENCES organization_memberships(id),
  reviewed_at timestamptz, review_note text,
  payment_id uuid REFERENCES payments(id),       -- set on confirmation
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX payment_claims_org_state ON payment_claims (organization_id, state);
-- Dedupe "same remittance reported twice": a bank/ussd ref is unique per
-- member regardless of which drive/obligation it targets. NULL refs (cash
-- claims with no reference) never dedupe. The spec tuple
-- (membership,drive,ref) is covered — this is strictly tighter.
CREATE UNIQUE INDEX payment_claims_member_ref
  ON payment_claims (membership_id, external_ref) WHERE external_ref IS NOT NULL;
ALTER TABLE payments ADD CONSTRAINT payments_claim_fk
  FOREIGN KEY (claim_id) REFERENCES payment_claims(id);
