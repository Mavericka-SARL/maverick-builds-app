-- Test-schema mirror of migrations/110_metric_picklist.sql: a pick-list
-- metric's dimension.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS picklist_dimension_id UUID;
