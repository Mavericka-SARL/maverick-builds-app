-- Mirrors migration 056: workflow defs and automation rules are
-- revision-scoped. No FK here — the minimal test schema has no
-- model.revision table; production enforces it.

ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS revision_id UUID;

ALTER TABLE workflow.workflow_def DROP CONSTRAINT IF EXISTS workflow_def_application_id_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS workflow_def_rev_name_uq
    ON workflow.workflow_def (application_id, revision_id, name) NULLS NOT DISTINCT;

ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS revision_id UUID;

ALTER TABLE workflow.automation_rule DROP CONSTRAINT IF EXISTS automation_rule_application_id_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS automation_rule_rev_name_uq
    ON workflow.automation_rule (application_id, revision_id, name) NULLS NOT DISTINCT;
