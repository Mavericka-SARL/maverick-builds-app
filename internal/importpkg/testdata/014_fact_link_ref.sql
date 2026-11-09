-- Test-schema mirror of migrations/169_model_link_cell_provenance.sql: the
-- model link a typed value was written by.
ALTER TABLE runtime.fact_input ADD COLUMN IF NOT EXISTS link_ref UUID;
