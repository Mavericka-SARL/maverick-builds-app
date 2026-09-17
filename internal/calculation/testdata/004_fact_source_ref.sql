-- Mirror migrations/035_form_metric_mapping.sql's additions that
-- LoadInputValueMap now depends on: fact_input.source_ref (form/import-
-- sourced rows) and the form_metric_mapping table its 'replace'/'last'
-- coverage check joins against. Minimal columns only — just enough for the
-- store's queries to run against this package's snapshot schema.
ALTER TABLE runtime.fact_input ADD COLUMN IF NOT EXISTS source_ref UUID;
CREATE INDEX IF NOT EXISTS fact_input_source_ref_idx ON runtime.fact_input (source_ref) WHERE source_ref IS NOT NULL;

CREATE TABLE IF NOT EXISTS model.form_metric_mapping (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    aggregation TEXT NOT NULL DEFAULT 'sum'
);
