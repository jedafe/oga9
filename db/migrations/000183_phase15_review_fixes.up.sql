-- Ref: oga9-engineering/sections/23-integrations.md + 16/17 — review fixes.
-- 1) webhook_receipts gains organization_id so the verifying connector's
--    org attribution persists (ExternalEventReceived can route scoped).
-- 2) §16/§17 spec FKs carry no cascade — restore plain REFERENCES on the
--    child tables (spec reserves ON DELETE CASCADE for join/state rows
--    like welfare_visit_delegates / chat_channel_members).
-- 3) elections manifest publishes gains CampaignStarted (the nominations→
--    campaign transition emits it — catalogued this round).

ALTER TABLE webhook_receipts
  ADD COLUMN organization_id uuid REFERENCES organizations(id);
CREATE INDEX webhook_receipts_org ON webhook_receipts (organization_id)
  WHERE organization_id IS NOT NULL;

ALTER TABLE discipline_hearings
  DROP CONSTRAINT discipline_hearings_case_id_fkey,
  ADD CONSTRAINT discipline_hearings_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES discipline_cases(id);
ALTER TABLE discipline_appeals
  DROP CONSTRAINT discipline_appeals_case_id_fkey,
  ADD CONSTRAINT discipline_appeals_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES discipline_cases(id);
ALTER TABLE welfare_case_events
  DROP CONSTRAINT welfare_case_events_case_id_fkey,
  ADD CONSTRAINT welfare_case_events_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id);
ALTER TABLE welfare_approvals
  DROP CONSTRAINT welfare_approvals_case_id_fkey,
  ADD CONSTRAINT welfare_approvals_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id);
ALTER TABLE welfare_visits
  DROP CONSTRAINT welfare_visits_case_id_fkey,
  ADD CONSTRAINT welfare_visits_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id);
ALTER TABLE welfare_benefits
  DROP CONSTRAINT welfare_benefits_case_id_fkey,
  ADD CONSTRAINT welfare_benefits_case_id_fkey
    FOREIGN KEY (case_id) REFERENCES welfare_cases(id);

UPDATE app_registry SET manifest = jsonb_set(
  manifest::jsonb, '{events,publishes}',
  (manifest::jsonb->'events'->'publishes') || '"CampaignStarted"'::jsonb)::json
WHERE app_id = 'elections';
