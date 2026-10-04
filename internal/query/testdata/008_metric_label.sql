-- Test-schema mirror of migrations/108_metric_label.sql: a metric's display
-- label, which the chart resolver reads.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS label TEXT;
