-- Test-schema mirror of migrations/109_member_formula.sql: a calculated
-- member's formula (no value is stored or rolled up at it).
ALTER TABLE model.dimension_member ADD COLUMN IF NOT EXISTS formula TEXT;
