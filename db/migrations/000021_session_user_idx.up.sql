CREATE INDEX user_sessions_user_active ON user_sessions (user_id, created_at DESC) WHERE revoked_at IS NULL;
