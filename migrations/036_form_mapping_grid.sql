-- Optional grid association on form-metric mappings.
-- Used as a UI hint: filtering available metrics to those shown in the selected grid.
ALTER TABLE model.form_metric_mapping
  ADD COLUMN IF NOT EXISTS grid_id UUID REFERENCES model.grid_def(id) ON DELETE SET NULL;
