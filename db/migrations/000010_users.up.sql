-- Ref: oga9-engineering/sections/02-identity-authentication-sessions.md
CREATE TABLE users (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  email citext UNIQUE NOT NULL,
  password_hash text,
  display_name text NOT NULL,
  japaconnect_user_id uuid,
  is_verified boolean NOT NULL DEFAULT false,
  mfa_secret bytea,
  status text NOT NULL DEFAULT 'active',
  created_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz
);
