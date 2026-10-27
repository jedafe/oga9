-- Ref: oga9-engineering/sections/15-subscription-admin.md — plan changes:
-- upgrades immediate (proration via provider), downgrades scheduled at
-- period end. scheduled_plan_slug holds the pending downgrade; the
-- subscription sweep applies it when current_period_end passes.
ALTER TABLE subscriptions
  ADD COLUMN scheduled_plan_slug text REFERENCES plans(slug);
