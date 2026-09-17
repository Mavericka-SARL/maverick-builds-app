-- Mirror of migration 076 for the in-process test database.
ALTER TABLE workflow.workflow_step
    ADD COLUMN IF NOT EXISTS rework_count INT NOT NULL DEFAULT 0;
