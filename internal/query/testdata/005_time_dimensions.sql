-- Mirror migrations/087_time_dimensions.sql for the columns the chart
-- resolver reads: dimension type + granularity, member time_index, metric
-- time_summary.
ALTER TABLE model.dimension_def
  ADD COLUMN IF NOT EXISTS dimension_type TEXT NOT NULL DEFAULT 'standard',
  ADD COLUMN IF NOT EXISTS time_granularity TEXT,
  ADD COLUMN IF NOT EXISTS fiscal_year_start_month SMALLINT;

ALTER TABLE model.dimension_member
  ADD COLUMN IF NOT EXISTS period_start DATE,
  ADD COLUMN IF NOT EXISTS period_end DATE,
  ADD COLUMN IF NOT EXISTS time_index INT;

ALTER TABLE model.metric_def
  ADD COLUMN IF NOT EXISTS time_summary TEXT NOT NULL DEFAULT 'sum';
