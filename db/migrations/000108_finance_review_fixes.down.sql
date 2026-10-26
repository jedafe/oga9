UPDATE app_registry SET manifest = jsonb_set(manifest, '{permissions}',
  '["org.finance.dues.manage","org.finance.payments.record","org.finance.payments.approve","org.finance.reports.read"]')
WHERE app_id = 'finance_lite';

DROP INDEX membership_renewals_payment_uq;

DROP INDEX payment_claims_member_ref;
CREATE UNIQUE INDEX payment_claims_member_ref
  ON payment_claims (membership_id, external_ref) WHERE external_ref IS NOT NULL;
