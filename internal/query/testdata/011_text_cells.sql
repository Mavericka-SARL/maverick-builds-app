-- Test-schema mirror of migrations/111_text_cells_highlights_member_upkeep.sql:
-- text cells, highlight rules and business-maintained dimensions.
ALTER TABLE runtime.fact_input ADD COLUMN IF NOT EXISTS text_value TEXT;
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS highlight_rules JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE model.dimension_def ADD COLUMN IF NOT EXISTS business_maintained BOOLEAN NOT NULL DEFAULT false;
