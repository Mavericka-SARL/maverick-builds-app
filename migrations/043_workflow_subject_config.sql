-- Subject config stores the specific entity this workflow operates on.
-- Shape by subject_type:
--   grid:         {"grid_id": "<grid_def_id>"}
--   grid_metric:  {"grid_id": "<grid_def_id>", "metric_id": "<metric_def_id>"}
--   form_record:  {"form_id": "<form_def_id>"}
--   form_records: {"form_id": "<form_def_id>"}
ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS subject_config JSONB NOT NULL DEFAULT '{}';

-- Extend dashboard_widget to recognise 'workflow_action' as a valid widget type.
COMMENT ON COLUMN model.dashboard_widget.widget_type IS
    'grid | form | automation_button | integration_button | text | workflow_action';
