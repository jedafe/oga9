-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
CREATE TABLE usage_meters (                     -- metering for limits
  tenant_id uuid NOT NULL REFERENCES tenants(id),
  limit_key text NOT NULL, used bigint NOT NULL DEFAULT 0,
  period_start date NOT NULL,
  PRIMARY KEY (tenant_id, limit_key, period_start)
);
