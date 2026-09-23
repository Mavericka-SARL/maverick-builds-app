-- Mirror migrations/087_time_dimensions.sql for the columns the model
-- store's typed dimension / period member / time summary writers touch.
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
