-- agg_rule 'rate' — Anaplan's Ratio summary method.
--
-- 'rate' was already selectable in the console but the engine had no case for
-- it, and rollup.combineAgg falls through to summing anything it does not
-- recognise, so choosing it silently produced a sum. Making it real needs two
-- operands: the parent total is numerator_total / denominator_total, taken
-- from two other metrics rather than from this metric's own member values.
--
-- That is what distinguishes it from 'formula' (added alongside): formula
-- re-evaluates this metric's own expression against aggregated inputs, whereas
-- rate divides two nominated metrics and so works on input metrics too, which
-- have no expression to re-evaluate. A price typed per product totals as
-- revenue/volume, not as a sum or a mean of the prices.
--
-- ON DELETE SET NULL rather than CASCADE: deleting the numerator must not
-- delete every metric that referenced it. The rule then has an incomplete
-- operand pair, which validateAggRule surfaces on the next save.
ALTER TABLE model.metric_def
    ADD COLUMN IF NOT EXISTS agg_numerator_metric_id   UUID REFERENCES model.metric_def(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS agg_denominator_metric_id UUID REFERENCES model.metric_def(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS metric_def_agg_numerator_idx   ON model.metric_def (agg_numerator_metric_id);
CREATE INDEX IF NOT EXISTS metric_def_agg_denominator_idx ON model.metric_def (agg_denominator_metric_id);
