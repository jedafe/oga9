-- Ref: oga9-engineering/conventions/system-settings-catalog.md
-- Platform-level defaults (tenant_id IS NULL semantics — key is PK).
INSERT INTO system_settings (key, value) VALUES
  ('platform.maintenance_mode', 'false'),
  ('platform.registration_open', 'true'),
  ('billing.grace_period_days', '14'),
  ('license.offline_grace_days', '30')
ON CONFLICT (key) DO NOTHING;
