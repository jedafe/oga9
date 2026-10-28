DROP INDEX IF EXISTS organizations_custom_domain;
ALTER TABLE organizations DROP COLUMN custom_domain, DROP COLUMN description;
DROP INDEX IF EXISTS payment_claims_idempotency;
ALTER TABLE payment_claims DROP COLUMN idempotency_key;
ALTER TABLE tenants DROP COLUMN invoice_no_seq;
DROP TABLE platform_invoices;
