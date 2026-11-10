-- 175: a dimension property's label (owner's decision 2026-10-06, confirmed
-- 2026-11-10). An import turns a column header into an identifier formulas
-- can name ("Sales Segment" → sales_segment) and keeps the header, as the
-- person wrote it, as the property's label; the console shows the label,
-- formulas and the API keep naming the property by its name. NULL: the name
-- is shown. A developer sets or clears it.
ALTER TABLE model.dimension_property ADD COLUMN IF NOT EXISTS label text;
