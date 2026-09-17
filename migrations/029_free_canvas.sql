-- Add free-canvas pixel-position columns to dashboard_widget.
-- pos_x/pos_y are top-left corner in px; size_w/size_h are dimensions in px.
ALTER TABLE model.dashboard_widget
  ADD COLUMN IF NOT EXISTS pos_x  INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS pos_y  INT NOT NULL DEFAULT 0,
  ADD COLUMN IF NOT EXISTS size_w INT NOT NULL DEFAULT 600,
  ADD COLUMN IF NOT EXISTS size_h INT NOT NULL DEFAULT 200;

-- Migrate existing grid-based positions to pixel equivalents.
-- 12-column grid with ~100px per column; rows spaced 220px apart.
UPDATE model.dashboard_widget SET
  pos_x  = (GREATEST(col_start, 1) - 1) * 100,
  pos_y  = sort_order * 220,
  size_w = GREATEST(col_span, 1) * 100,
  size_h = 200;
