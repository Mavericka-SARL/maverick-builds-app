-- Free-form tags on grids, the shape dashboards (023), dimensions and
-- metrics (098) carry: the developer console filters its Grids list by them.
-- A tag is a label on the definition row, so it travels with it: revision
-- duplication and model export/import copy it.

ALTER TABLE model.grid_def ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
