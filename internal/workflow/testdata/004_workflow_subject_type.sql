-- Mirrors migrations/042_workflow_subject_type.sql for integration tests.
ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS subject_type TEXT NOT NULL DEFAULT '';
