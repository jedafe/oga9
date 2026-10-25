-- Ref: oga9-engineering/sections/11-members.md (group 9)
CREATE TABLE membership_renewals (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  membership_id uuid NOT NULL REFERENCES organization_memberships(id),
  period_label text NOT NULL,                  -- '2026' / '2026-Q1'
  starts_at date NOT NULL, ends_at date NOT NULL,
  payment_id uuid,                             -- -> finance later (no FK, MIGRATION-ORDER)
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(membership_id, period_label)
);

-- §11 additions to the §04 base: dues standing + last renewal stamp.
ALTER TABLE organization_memberships
  ADD COLUMN dues_standing text NOT NULL DEFAULT 'good',
  ADD COLUMN last_renewed_at timestamptz;

-- Per-org member_no sequence (org.membership.number_format consumer) —
-- atomic UPDATE ... +1 RETURNING; numbers are never reused.
ALTER TABLE organizations ADD COLUMN member_no_seq bigint NOT NULL DEFAULT 0;
