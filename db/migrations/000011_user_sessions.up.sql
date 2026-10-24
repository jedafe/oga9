CREATE TABLE user_sessions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  jti uuid NOT NULL UNIQUE,
  device_label text,
  ip inet, user_agent text,
  expires_at timestamptz NOT NULL,
  revoked_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX user_sessions_user_active ON user_sessions (user_id, created_at DESC) WHERE revoked_at IS NULL;
