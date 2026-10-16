-- Drop the per-model reporting views and the log of the SQL that made them.
--
-- internal/schemamigration created runtime.m_<model>_inputs and
-- runtime.m_<model>_results after every metric or dimension change and every
-- activation, and kept each generated file as a row of
-- deployment.schema_migration. Nothing read the views, and the inputs view
-- was wrong: it summed every append-only fact_input row of a cell, so a cell
-- typed 100 and corrected to 105 read 205, and its revision column was always
-- blank because fact_input.revision_name is no longer written. The generator
-- and its two developer routes are removed with this migration. The
-- deployment schema stays, empty.
DO $$
DECLARE
    v record;
BEGIN
    FOR v IN
        SELECT schemaname, viewname FROM pg_views
        WHERE schemaname = 'runtime' AND viewname ~ '^m_[0-9a-f]+_(inputs|results)$'
    LOOP
        EXECUTE format('DROP VIEW IF EXISTS %I.%I', v.schemaname, v.viewname);
    END LOOP;
END $$;

DROP TABLE IF EXISTS deployment.schema_migration;
