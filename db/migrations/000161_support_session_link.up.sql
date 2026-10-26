-- Ref: oga9-engineering/sections/15-subscription-admin.md — link operator
-- support sessions to the session row so listings/banners can resolve it.
ALTER TABLE user_sessions
  ADD COLUMN support_session_id uuid REFERENCES support_sessions(id);
