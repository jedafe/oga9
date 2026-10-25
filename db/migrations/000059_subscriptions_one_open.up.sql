-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md
-- One open (non-cancelled) subscription per tenant — closes the
-- read-then-insert race on POST /billing/subscription so a cancelled
-- history can't fork into parallel "current" rows.
CREATE UNIQUE INDEX subscriptions_one_open ON subscriptions (tenant_id)
  WHERE state <> 'cancelled';
