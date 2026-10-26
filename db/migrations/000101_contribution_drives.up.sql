-- Ref: oga9-engineering/sections/12-finance-lite.md — "the call": org
-- contribution drives (voluntary or fixed-amount) with a transparency ledger.
CREATE TABLE contribution_drives (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  title text NOT NULL,
  purpose text,
  kind text NOT NULL DEFAULT 'voluntary',     -- voluntary|fixed_amount
  fixed_amount_minor bigint,                  -- required when kind=fixed_amount
  target_amount_minor bigint,                 -- goal for progress display
  benefit_member_id uuid REFERENCES organization_memberships(id), -- null = org-general
  contribution_type_id uuid REFERENCES contribution_types(id),
  opens_at timestamptz NOT NULL DEFAULT now(), closes_at timestamptz,
  state text NOT NULL DEFAULT 'open',          -- open|closed|disbursed|cancelled
  created_by uuid NOT NULL REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  CHECK (kind <> 'fixed_amount' OR fixed_amount_minor IS NOT NULL)
);
CREATE INDEX contribution_drives_org_state ON contribution_drives (organization_id, state);
CREATE INDEX contribution_drives_open_close ON contribution_drives (closes_at)
  WHERE state = 'open' AND closes_at IS NOT NULL;
