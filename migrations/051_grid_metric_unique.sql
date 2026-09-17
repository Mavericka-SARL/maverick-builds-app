-- A metric may only belong to one grid at a time. metric_def rows are already
-- revision-scoped (each revision has its own copies), so a plain UNIQUE on
-- metric_id is revision-safe: it cannot collide across revisions.
DO $$
BEGIN
    IF EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'grid_metric_grid_id_metric_id_key'
    ) THEN
        ALTER TABLE model.grid_metric DROP CONSTRAINT grid_metric_grid_id_metric_id_key;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'grid_metric_metric_id_key'
    ) THEN
        ALTER TABLE model.grid_metric ADD CONSTRAINT grid_metric_metric_id_key UNIQUE (metric_id);
    END IF;
END $$;
