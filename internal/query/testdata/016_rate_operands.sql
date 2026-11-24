-- Test-schema mirror of a rate metric's operands (agg_rule 'rate'): the
-- chart resolver reads a rate metric above its leaves as its numerator over
-- its denominator.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS agg_numerator_metric_id UUID REFERENCES model.metric_def(id) ON DELETE SET NULL;
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS agg_denominator_metric_id UUID REFERENCES model.metric_def(id) ON DELETE SET NULL;
