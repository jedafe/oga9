-- Ref: oga9-engineering/adr/ADR-016-rules-workflow-engine.md (group 26)
-- Org-configurable rules: a rule_definitions row may pin an org_settings
-- key ({"value": ...}) or bound it ({"min":..,"max":..,"default":..}).
-- NULL organization_id = platform/template default rows; resolution order
-- is org rule → org_settings → catalog default (Rule 23). Evaluation is
-- logged for auditability (ADR-016 consequence).
CREATE TABLE rule_definitions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid REFERENCES organizations(id),
  key text NOT NULL,                            -- the org_settings-style key it governs
  kind text NOT NULL DEFAULT 'value',           -- value|threshold|window|chain|schedule
  definition jsonb NOT NULL,
  active boolean NOT NULL DEFAULT true,
  priority int NOT NULL DEFAULT 0,              -- highest wins when several rules bind a key
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX rule_definitions_org_key
  ON rule_definitions (organization_id, key) WHERE organization_id IS NOT NULL;
CREATE UNIQUE INDEX rule_definitions_platform_key
  ON rule_definitions (key) WHERE organization_id IS NULL;
CREATE INDEX rule_definitions_org ON rule_definitions (organization_id) WHERE active;
