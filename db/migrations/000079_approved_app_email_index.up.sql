-- Ref: oga9-engineering/sections/11-members.md
-- The registration hook looks up approved pre-signup applications by
-- email on every login/verify — keep that lookup indexed (citext gives
-- case-insensitive equality natively).
CREATE INDEX membership_apps_approved_email
  ON membership_applications (email)
  WHERE user_id IS NULL AND status = 'approved';
