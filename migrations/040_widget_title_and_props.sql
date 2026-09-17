-- Per-widget optional visible title and flexible style properties
ALTER TABLE model.dashboard_widget
  ADD COLUMN IF NOT EXISTS title       TEXT,
  ADD COLUMN IF NOT EXISTS show_title  BOOLEAN NOT NULL DEFAULT FALSE,
  ADD COLUMN IF NOT EXISTS widget_props JSONB;
