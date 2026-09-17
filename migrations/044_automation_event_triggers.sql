-- Extend automation rules with event-driven trigger support.
--
-- New trigger types: form_approval fires when a form record reaches an
-- approved/closed status; grid_change fires on any cell writeback.
--
-- workflow_def_id allows rules to reference a workflow directly by ID
-- rather than by name, so renames no longer break existing rules.
--
-- source_form_id / source_grid_id scope a rule to a specific form or grid;
-- NULL means "fire for any form/grid event in this application".

ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'form_approval';
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'grid_change';

ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS workflow_def_id UUID REFERENCES workflow.workflow_def(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS source_form_id  UUID,
    ADD COLUMN IF NOT EXISTS source_grid_id  UUID;
