ALTER TABLE webhook_receipts DROP COLUMN organization_id;

ALTER TABLE discipline_hearings
  DROP CONSTRAINT discipline_hearings_case_id_fkey,
  ADD CONSTRAINT discipline_hearings_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES discipline_cases(id) ON DELETE CASCADE;
ALTER TABLE discipline_appeals
  DROP CONSTRAINT discipline_appeals_case_id_fkey,
  ADD CONSTRAINT discipline_appeals_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES discipline_cases(id) ON DELETE CASCADE;
ALTER TABLE welfare_case_events
  DROP CONSTRAINT welfare_case_events_case_id_fkey,
  ADD CONSTRAINT welfare_case_events_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id) ON DELETE CASCADE;
ALTER TABLE welfare_approvals
  DROP CONSTRAINT welfare_approvals_case_id_fkey,
  ADD CONSTRAINT welfare_approvals_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id) ON DELETE CASCADE;
ALTER TABLE welfare_visits
  DROP CONSTRAINT welfare_visits_case_id_fkey,
  ADD CONSTRAINT welfare_visits_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id) ON DELETE CASCADE;
ALTER TABLE welfare_benefits
  DROP CONSTRAINT welfare_benefits_case_id_fkey,
  ADD CONSTRAINT welfare_benefits_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id) ON DELETE CASCADE;
