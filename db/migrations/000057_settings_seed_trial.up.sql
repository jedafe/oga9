-- Ref: oga9-engineering/conventions/system-settings-catalog.md
INSERT INTO system_settings (key, value) VALUES
  ('billing.trial_days', '14')
ON CONFLICT (key) DO NOTHING;
