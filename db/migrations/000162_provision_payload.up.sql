-- Ref: oga9-engineering/sections/15-subscription-admin.md — idempotent
-- provision needs the request body to detect request_id reuse with a
-- DIFFERENT intent (Rule 9-style key mismatch).
ALTER TABLE tenant_provisions ADD COLUMN payload jsonb;
