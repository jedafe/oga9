-- Ref: oga9-engineering/conventions/system-settings-catalog.md
-- Platform-level settings (Rule 3: hot-reloadable via GetSetting).

-- name: GetSystemSetting :one
SELECT value FROM system_settings WHERE key = $1;
