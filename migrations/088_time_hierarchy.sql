-- Time dimensions may carry a period hierarchy (Q1 → H1 → FY26).
--
-- A LEAF period carries period_start/period_end and the server-owned
-- time_index; time-series functions move along leaves only. A parent
-- period (H1, FY26) is an aggregate: no dates, no ordinal — its value is its
-- descendants reduced by the metric's time_summary. The "time members have
-- no parent" rule of 087 is therefore lifted; dimension_member_period_ck
-- (dates and ordinal all-or-nothing) still distinguishes the two kinds.
ALTER TABLE model.dimension_member DROP CONSTRAINT IF EXISTS dimension_member_time_flat_ck;
