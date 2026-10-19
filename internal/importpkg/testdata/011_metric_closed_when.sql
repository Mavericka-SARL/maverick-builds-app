-- Mirrors migrations/147_metric_closed_when.sql: the cells an input metric
-- closes to input. The import commit reads it for its staged metrics.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS closed_when jsonb NOT NULL DEFAULT '[]'::jsonb;
