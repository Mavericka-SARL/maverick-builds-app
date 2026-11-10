-- 174: how a dimension's members total (owner's decision, 2026-11-09).
--
-- A dimension of scenarios (Base, Downside) was added up like any other:
-- an unpinned KPI, a chart without its selector, a query without a filter
-- and every stored total read Base + Downside. A dimension now has a total
-- rule, which every metric's totals over it follow unless the metric sets
-- its own for that dimension:
--   sum    its members add up — each metric by its own total rule (agg_rule,
--          time_summary): the default, and every dimension's until now;
--   first  a total is the value at its first member (Build's order): Base;
--   last   a total is the value at its last member;
--   none   no total: the members are alternatives, a read names one.
-- A time dimension's totals stay each metric's time summary.
--
-- dimension_def.agg_rule held this name since migration 018 and nothing
-- read it (the API stored whatever it was sent): every dimension starts as
-- sum, so no total changes until a developer sets a rule.
UPDATE model.dimension_def SET agg_rule = 'sum' WHERE agg_rule IS DISTINCT FROM 'sum';
ALTER TABLE model.dimension_def DROP CONSTRAINT IF EXISTS dimension_def_agg_rule_check;
ALTER TABLE model.dimension_def ADD CONSTRAINT dimension_def_agg_rule_check
	CHECK (agg_rule IN ('sum', 'first', 'last', 'none'));

-- A metric's own rule over one of its dimensions, overriding the
-- dimension's: a headcount that does add up across a dimension of
-- alternatives, or a metric that totals as the first member where the
-- dimension adds up. Goes with the metric or the dimension.
CREATE TABLE IF NOT EXISTS model.metric_dimension_rule (
	metric_id    uuid NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
	dimension_id uuid NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
	rule         text NOT NULL CHECK (rule IN ('sum', 'first', 'last', 'none')),
	PRIMARY KEY (metric_id, dimension_id)
);
CREATE INDEX IF NOT EXISTS metric_dimension_rule_dimension ON model.metric_dimension_rule (dimension_id);
