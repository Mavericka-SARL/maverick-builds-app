-- Mirror of migration 074 for the in-process test database.
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'integration_completed';
ALTER TYPE workflow.trigger_type ADD VALUE IF NOT EXISTS 'integration_failed';

ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS source_integration_id UUID;

ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS single_active_instance BOOLEAN NOT NULL DEFAULT true;
