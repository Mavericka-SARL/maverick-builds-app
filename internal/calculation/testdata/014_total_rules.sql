-- Test-schema mirror of migration 174: a dimension's total rule (agg_rule:
-- sum, first, last, none) and a metric's own rule over a dimension, which
-- calculation.LoadTotalRules reads with every dimension load.
ALTER TABLE model.dimension_def ADD COLUMN IF NOT EXISTS agg_rule TEXT NOT NULL DEFAULT 'sum';
CREATE TABLE IF NOT EXISTS model.metric_dimension_rule (
    metric_id    UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    dimension_id UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    rule         TEXT NOT NULL,
    PRIMARY KEY (metric_id, dimension_id)
);
