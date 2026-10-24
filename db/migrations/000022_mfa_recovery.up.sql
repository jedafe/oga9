-- MFA recovery codes: hashed one-time codes (§02 mfa lifecycle).
ALTER TABLE users ADD COLUMN mfa_recovery_codes jsonb NOT NULL DEFAULT '[]';
