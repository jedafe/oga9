-- Backfill is additive and idempotent; removing seeded installs would
-- break existing tenants, so down is a no-op.
SELECT 1;
