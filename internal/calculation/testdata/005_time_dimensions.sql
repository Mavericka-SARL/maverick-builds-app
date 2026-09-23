-- Mirror migrations/087_time_dimensions.sql: the columns the scheduler's
-- time-series path reads (dimension type + granularity, member periods and
-- ordinal, metric time summary, dependency offsets). Constraints are kept
-- so the fixtures exercise the same invariants as production.
ALTER TABLE model.dimension_def
  ADD COLUMN IF NOT EXISTS dimension_type TEXT NOT NULL DEFAULT 'standard',
  ADD COLUMN IF NOT EXISTS time_granularity TEXT,
  ADD COLUMN IF NOT EXISTS fiscal_year_start_month SMALLINT;

ALTER TABLE model.dimension_member
  ADD COLUMN IF NOT EXISTS period_start DATE,
  ADD COLUMN IF NOT EXISTS period_end DATE,
  ADD COLUMN IF NOT EXISTS time_index INT;

ALTER TABLE model.dimension_member
  ADD CONSTRAINT dimension_member_time_index_uq
    UNIQUE (dimension_id, time_index) DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT dimension_member_period_start_uq
    UNIQUE (dimension_id, period_start) DEFERRABLE INITIALLY DEFERRED;

ALTER TABLE model.metric_def
  ADD COLUMN IF NOT EXISTS time_summary TEXT NOT NULL DEFAULT 'sum';

ALTER TABLE model.calc_dependency
  ADD COLUMN IF NOT EXISTS min_time_offset INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS max_time_offset INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS unbounded_past BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS unbounded_future BOOLEAN NOT NULL DEFAULT false;
