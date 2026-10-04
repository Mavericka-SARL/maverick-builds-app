-- A metric's display label, shown wherever the metric is captioned (grid
-- rows and columns, chart legends, KPI tiles, exports by label). NULL keeps
-- the label derived from the technical name (gross_margin → "Gross Margin").
-- Finance statements need labels a technical name cannot carry: "R&D",
-- "G&A", "EBITDA Margin", the same "Net Revenue" caption on two blocks.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS label TEXT;
