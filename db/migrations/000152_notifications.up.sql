-- Ref: oga9-engineering/sections/14-comms-documents.md — in-app
-- notifications + per-user channel preferences.
CREATE TABLE notifications (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id uuid NOT NULL REFERENCES users(id),
  organization_id uuid REFERENCES organizations(id),
  kind text NOT NULL,                              -- seeded kinds
  title text NOT NULL, body text,
  ref_type text, ref_id uuid,                      -- deep-link target
  channel text NOT NULL DEFAULT 'in_app',          -- in_app|email|push
  read_at timestamptz, delivered_at timestamptz,
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX notifications_user_unread ON notifications (user_id, created_at DESC)
  WHERE read_at IS NULL;
CREATE INDEX notifications_org ON notifications (organization_id) WHERE organization_id IS NOT NULL;

CREATE TABLE notification_preferences (
  user_id uuid NOT NULL REFERENCES users(id),
  kind text NOT NULL, channel text NOT NULL,
  enabled boolean NOT NULL DEFAULT true,
  PRIMARY KEY (user_id, kind, channel)
);
