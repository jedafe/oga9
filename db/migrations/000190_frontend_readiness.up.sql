-- Ref: oga9-engineering/sections/15-subscription-admin.md (platform_invoices, g13 remainder)
-- Ref: oga9-engineering/sections/12-finance-lite.md (claims Idempotency-Key, Rule 9)
-- Ref: oga9-engineering/sections/04-tenancy-organizations.md (portal profile + custom domain)

CREATE TABLE platform_invoices (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  subscription_id uuid REFERENCES subscriptions(id),
  addon_slug text,
  number text NOT NULL,                        -- INV-{seq}
  amount_minor bigint NOT NULL, currency text NOT NULL DEFAULT 'USD',
  state text NOT NULL DEFAULT 'issued',        -- issued|paid|void
  period_start timestamptz, period_end timestamptz,
  issued_at timestamptz NOT NULL DEFAULT now(),
  paid_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX platform_invoices_number ON platform_invoices (tenant_id, number);
CREATE INDEX platform_invoices_tenant ON platform_invoices (tenant_id, issued_at DESC);
ALTER TABLE tenants ADD COLUMN invoice_no_seq bigint NOT NULL DEFAULT 0;

ALTER TABLE payment_claims ADD COLUMN idempotency_key text;
CREATE UNIQUE INDEX payment_claims_idempotency ON payment_claims (idempotency_key)
  WHERE idempotency_key IS NOT NULL;

ALTER TABLE organizations ADD COLUMN description text;
ALTER TABLE organizations ADD COLUMN custom_domain text;
CREATE UNIQUE INDEX organizations_custom_domain ON organizations (lower(custom_domain))
  WHERE custom_domain IS NOT NULL;
