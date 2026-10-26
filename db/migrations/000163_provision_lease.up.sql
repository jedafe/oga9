-- Ref: oga9-engineering/sections/15-subscription-admin.md — provision
-- lease stamping: the stale-claim check must measure from the last claim,
-- not row creation, so a live worker past the window isn't double-run.
ALTER TABLE tenant_provisions ADD COLUMN processing_started_at timestamptz;
