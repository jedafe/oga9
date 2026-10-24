DELETE FROM system_settings WHERE key IN (
  'platform.maintenance_mode',
  'platform.registration_open',
  'billing.grace_period_days',
  'license.offline_grace_days'
);
