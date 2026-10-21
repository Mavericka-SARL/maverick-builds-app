-- Calculated rows no longer carry the partition key of the pass that wrote
-- them. Nothing read it: it was a text of 120-odd characters (model,
-- revision, metric and month spelled out) on every row with an index of its
-- own, about a seventh of a calculated cell's storage, and its only effect
-- was that every result row was rewritten at the first pass of each month.
-- The partition's state stays in runtime.metric_partition_state.
--
-- The column becomes optional and its index goes; new rows leave it empty,
-- which takes no space. It is not dropped: a gateway of the release before,
-- still running during a rolling restart or brought back by a rollback,
-- writes it, and must keep working. A later migration may drop it.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = 'runtime' AND table_name = 'calc_result' AND column_name = 'partition_key') THEN
        ALTER TABLE runtime.calc_result ALTER COLUMN partition_key DROP NOT NULL;
    END IF;
END $$;

DROP INDEX IF EXISTS runtime.calc_result_partition_key_idx;
