-- Text cells, cell highlights, business-maintained dimensions and promoted
-- AI sessions (found rebuilding a sales target-setting workbook, 2026-10-04).

-- A text metric's cell holds free text (a planner's comment, an owner): the
-- fact row carries it in text_value with value 0, so it stays append-only,
-- is copied with the row by revision copies, splits and on-approve copies,
-- and a delete archives it like any fact. NULL for every number.
ALTER TABLE runtime.fact_input ADD COLUMN IF NOT EXISTS text_value TEXT;
ALTER TABLE runtime.fact_input_history ADD COLUMN IF NOT EXISTS text_value TEXT;
ALTER TABLE import.import_staging ADD COLUMN IF NOT EXISTS text_value TEXT;

CREATE OR REPLACE FUNCTION runtime.archive_deleted_fact() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO runtime.fact_input_history
        (id, model_id, revision_id, metric_id, dim_members, value, entered_by, entered_at, source_ref, delete_reason, text_value)
    VALUES
        (OLD.id, OLD.model_id, OLD.revision_id, OLD.metric_id, OLD.dim_members, OLD.value, OLD.entered_by, OLD.entered_at, OLD.source_ref,
         COALESCE(current_setting('mvx.delete_reason', true), ''), OLD.text_value);
    RETURN OLD;
END $$;

-- Highlight rules: how a metric's cells are tinted, as a list of
-- {op, value | than, abs, metric, tone} (metricformula.CheckHighlightRules).
-- Metrics are named, as formulas name them, so copies need no remapping.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS highlight_rules JSONB NOT NULL DEFAULT '[]'::jsonb;

-- A dimension whose members business users add, rename and remove (a
-- planner naming a new strategic activity). Only a developer sets it.
ALTER TABLE model.dimension_def ADD COLUMN IF NOT EXISTS business_maintained BOOLEAN NOT NULL DEFAULT false;

-- Set when the session's draft is promoted: the session is finished, and
-- its next message is refused with "start a new session".
ALTER TABLE ai_assistant.session ADD COLUMN IF NOT EXISTS promoted_at TIMESTAMPTZ;
