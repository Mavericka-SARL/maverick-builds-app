ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS subject_config JSONB NOT NULL DEFAULT '{}';
