-- Mirror of migration 075 for the in-process test database.
ALTER TABLE workflow.workflow_instance
    ADD COLUMN IF NOT EXISTS steps_snapshot          JSONB,
    ADD COLUMN IF NOT EXISTS context_schema_snapshot JSONB;
