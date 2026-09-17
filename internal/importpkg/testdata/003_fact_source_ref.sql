-- runtime.fact_input.source_ref, mirroring production migration 035.
--
-- A fact with source_ref set was posted by a form-metric mapping rather than
-- entered or imported directly. CommitImport's full_reload branch excludes
-- those rows from its delete (dropping them would leave the form records that
-- produced them with nothing on the grid), so this package's curated schema
-- needs the column to exercise that path.
ALTER TABLE runtime.fact_input ADD COLUMN IF NOT EXISTS source_ref UUID;
CREATE INDEX IF NOT EXISTS fact_input_source_ref_idx ON runtime.fact_input (source_ref) WHERE source_ref IS NOT NULL;
