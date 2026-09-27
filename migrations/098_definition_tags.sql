-- Free-form tags on dimensions and metrics, the same shape dashboards have
-- carried since 023: the developer console filters its Dimensions and
-- Metrics lists by them. A tag is a label on the definition row, so it
-- travels with it: revision duplication and model export/import copy it.

ALTER TABLE model.dimension_def ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS tags TEXT[] NOT NULL DEFAULT '{}';
