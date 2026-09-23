-- Time dimensions: an explicit, immutable marker on a dimension, validated
-- period dates on its members, an axis-specific time summary on metrics,
-- and per-edge time offsets on the dependency graph.
--
-- A dimension is a time dimension only when the developer marks it as such at
-- creation. Names such as "month" or "period" never imply it. Existing
-- dimensions backfill as 'standard' and keep their behaviour.
--
-- See TIME_SERIES_FUNCTIONS_IMPLEMENTATION.md §3.

-- 3.1 Dimension fields ------------------------------------------------------

ALTER TABLE model.dimension_def
  ADD COLUMN IF NOT EXISTS dimension_type TEXT NOT NULL DEFAULT 'standard',
  ADD COLUMN IF NOT EXISTS time_granularity TEXT,
  ADD COLUMN IF NOT EXISTS fiscal_year_start_month SMALLINT;

ALTER TABLE model.dimension_def
  ADD CONSTRAINT dimension_def_type_ck
    CHECK (dimension_type IN ('standard', 'time')),
  ADD CONSTRAINT dimension_def_time_granularity_ck
    CHECK (time_granularity IS NULL OR time_granularity IN
      ('day', 'week', 'month', 'quarter', 'half_year', 'year', 'custom')),
  ADD CONSTRAINT dimension_def_fiscal_month_ck
    CHECK (fiscal_year_start_month IS NULL OR
           fiscal_year_start_month BETWEEN 1 AND 12),
  ADD CONSTRAINT dimension_def_time_config_ck
    CHECK (
      (dimension_type = 'standard' AND time_granularity IS NULL AND
       fiscal_year_start_month IS NULL)
      OR
      (dimension_type = 'time' AND time_granularity IS NOT NULL AND
       fiscal_year_start_month IS NOT NULL)
    ),
  -- Phase 1: a time dimension is flat and self-contained — no dimension
  -- hierarchy above it, no source dimension it is derived from.
  ADD CONSTRAINT dimension_def_time_no_hierarchy_ck
    CHECK (dimension_type = 'standard' OR
           (parent_dimension_id IS NULL AND source_dimension_id IS NULL));

-- The semantic type is immutable. A wrongly typed dimension is replaced,
-- never retyped in place: facts, formulas and dependency offsets were all
-- built against the original meaning.
CREATE OR REPLACE FUNCTION model.dimension_def_time_immutable() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  IF NEW.dimension_type IS DISTINCT FROM OLD.dimension_type
     OR NEW.time_granularity IS DISTINCT FROM OLD.time_granularity
     OR NEW.fiscal_year_start_month IS DISTINCT FROM OLD.fiscal_year_start_month THEN
    RAISE EXCEPTION 'dimension_type, time_granularity and fiscal_year_start_month are immutable after creation'
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

DROP TRIGGER IF EXISTS dimension_def_time_immutable ON model.dimension_def;
CREATE TRIGGER dimension_def_time_immutable
  BEFORE UPDATE ON model.dimension_def
  FOR EACH ROW EXECUTE FUNCTION model.dimension_def_time_immutable();

-- 3.2 Time-member fields -----------------------------------------------------

ALTER TABLE model.dimension_member
  ADD COLUMN IF NOT EXISTS period_start DATE,
  ADD COLUMN IF NOT EXISTS period_end DATE,
  ADD COLUMN IF NOT EXISTS time_index INT;

ALTER TABLE model.dimension_member
  ADD CONSTRAINT dimension_member_period_ck
    CHECK (
      (period_start IS NULL AND period_end IS NULL AND time_index IS NULL)
      OR
      (period_start IS NOT NULL AND period_end IS NOT NULL AND
       time_index IS NOT NULL AND time_index >= 0 AND
       period_start <= period_end)
    ),
  -- Phase 1: time members have no parent period.
  ADD CONSTRAINT dimension_member_time_flat_ck
    CHECK (period_start IS NULL OR parent_member_id IS NULL);

-- Uniqueness of the ordinal and of the start date within one dimension.
-- Deferrable constraints rather than partial unique indexes: the server
-- re-indexes every member of a dimension in one transaction whenever the
-- period set changes, and a non-deferrable index would reject the
-- intermediate state of that renumbering. NULLs (standard members) are
-- distinct under a unique constraint, so they never collide.
ALTER TABLE model.dimension_member
  ADD CONSTRAINT dimension_member_time_index_uq
    UNIQUE (dimension_id, time_index) DEFERRABLE INITIALLY DEFERRED,
  ADD CONSTRAINT dimension_member_period_start_uq
    UNIQUE (dimension_id, period_start) DEFERRABLE INITIALLY DEFERRED;

-- 3.3 Time summary method ----------------------------------------------------

ALTER TABLE model.metric_def
  ADD COLUMN IF NOT EXISTS time_summary TEXT NOT NULL DEFAULT 'sum',
  ADD CONSTRAINT metric_def_time_summary_ck
    CHECK (time_summary IN
      ('sum', 'average', 'min', 'max', 'first', 'last', 'none'));

-- 3.4 Dependency metadata ----------------------------------------------------

ALTER TABLE model.calc_dependency
  ADD COLUMN IF NOT EXISTS min_time_offset INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS max_time_offset INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS unbounded_past BOOLEAN NOT NULL DEFAULT false,
  ADD COLUMN IF NOT EXISTS unbounded_future BOOLEAN NOT NULL DEFAULT false;
