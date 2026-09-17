-- Mirror of migration 044 for the in-process test database.
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'form_approval';
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'grid_change';

ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS workflow_def_id UUID REFERENCES workflow.workflow_def(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS source_form_id  UUID,
    ADD COLUMN IF NOT EXISTS source_grid_id  UUID;
