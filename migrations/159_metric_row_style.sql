-- A metric's row style (production review, 2026-11-07): every calculated row
-- of a statement was the same bold blue, so EBITDA and EBIT read like any
-- other line and only capitals marked a subtotal. 'total' draws the metric's
-- row as a total wherever a grid shows it: its label bold and a rule above.
ALTER TABLE model.metric_def
    ADD COLUMN IF NOT EXISTS row_style text NOT NULL DEFAULT ''
        CHECK (row_style IN ('', 'total'));
