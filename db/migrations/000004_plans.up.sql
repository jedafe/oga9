-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
CREATE TABLE plans (
  slug text PRIMARY KEY,
  label text NOT NULL,
  price_monthly_minor bigint, price_annual_minor bigint,
  currency text NOT NULL DEFAULT 'USD',
  is_public boolean NOT NULL DEFAULT true,
  sort int NOT NULL
);
