-- Ref: oga9-engineering/sections/07-entitlements-subscriptions.md — the
-- license-expiry sweep marks rows when LicenseExpired is emitted (once per
-- expiry window; renewal resets it so the NEXT expiry re-emits).
ALTER TABLE licenses ADD COLUMN expired_notified boolean NOT NULL DEFAULT false;
