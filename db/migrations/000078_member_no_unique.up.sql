-- Ref: oga9-engineering/sections/11-members.md
-- member_no is the org-scoped public identifier — sequence-minted numbers
-- can't collide, but invite member_no_hint is caller-supplied; enforce
-- uniqueness per org (NULL = not yet minted).
CREATE UNIQUE INDEX memberships_member_no_uq
  ON organization_memberships (organization_id, member_no)
  WHERE member_no IS NOT NULL;
