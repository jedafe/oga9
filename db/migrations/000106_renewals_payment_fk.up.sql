-- Ref: oga9-engineering/schemas/MIGRATION-ORDER.md g12 — the payments table
-- exists now, so the renewal → payment link gets its FK (column was added
-- nullable in 000074 for ordering).
ALTER TABLE membership_renewals ADD CONSTRAINT membership_renewals_payment_fk
  FOREIGN KEY (payment_id) REFERENCES payments(id);
