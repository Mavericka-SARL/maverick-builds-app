-- Add subject_type to workflow_def so developers can declare what entity this workflow operates on.
-- Values: '' (unspecified), 'form_record', 'grid_row', 'metric_value', 'general'
ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS subject_type TEXT NOT NULL DEFAULT '';
