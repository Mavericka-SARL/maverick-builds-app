-- Mirrors migrations/066_metric_ratio_operands.sql. This package runs against
-- its own trimmed schema rather than the real migration set, so a column added
-- there has to be added here too or LoadModelMetrics fails to scan.
ALTER TABLE model.metric_def
    ADD COLUMN agg_numerator_metric_id   UUID REFERENCES model.metric_def(id) ON DELETE SET NULL,
    ADD COLUMN agg_denominator_metric_id UUID REFERENCES model.metric_def(id) ON DELETE SET NULL;
