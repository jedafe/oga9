-- Ref: oga9-engineering/sections/12-finance-lite.md — review fixes.
-- 1) Claim dedupe: a REJECTED claim must not burn the member's external_ref —
--    resubmitting corrected proof is a legitimate retry, not a duplicate.
DROP INDEX payment_claims_member_ref;
CREATE UNIQUE INDEX payment_claims_member_ref
  ON payment_claims (membership_id, external_ref)
  WHERE external_ref IS NOT NULL AND state <> 'rejected';

-- 2) One payment settles at most one renewal — concurrent renewals racing
--    the same payment_id hit this instead of double-linking.
CREATE UNIQUE INDEX membership_renewals_payment_uq
  ON membership_renewals (payment_id) WHERE payment_id IS NOT NULL;

-- 3) finance_lite manifest drift: declare the full §12 node surface
--    (000107 fixed comms_documents/organization_core; this app was missed).
UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.finance.obligations.manage","org.finance.payments.record","org.finance.payments.approve","org.finance.reconcile","org.finance.reports.read","org.finance.view_own","org.finance.statement_view","org.finance.dues.manage","org.finance.drives.manage","org.finance.claims.submit_own","org.finance.claims.review"]')
WHERE app_id = 'finance_lite';
