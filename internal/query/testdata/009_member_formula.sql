-- Test-schema mirror of migrations/109_member_formula.sql: a calculated
-- member's formula, which the chart resolver reads.
ALTER TABLE model.dimension_member ADD COLUMN IF NOT EXISTS formula TEXT;
