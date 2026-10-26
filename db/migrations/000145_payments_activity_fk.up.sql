-- Ref: oga9-engineering/schemas/MIGRATION-ORDER.md — g10 landed after
-- finance, so payments.activity_id was left untyped in 000103; now that
-- activities exists the FK is restored (stub → ALTER pattern).
ALTER TABLE payments ADD CONSTRAINT payments_activity_fk
  FOREIGN KEY (activity_id) REFERENCES activities(id);
