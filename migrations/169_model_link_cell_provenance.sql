-- A value a model link wrote names the link (2026-11-09).
--
-- A model link imports with replace: each run overwrites the cells its
-- source holds a value for. A source cell emptied since the last run gives
-- no record, so the target kept its last copy, and nothing recorded which
-- target cells earlier runs had written. A typed row a link writes now
-- carries the link's integration_def.link_id (the same in every revision of
-- the model, migration 126), and a run clears the cells whose latest value
-- that link wrote and that its source no longer holds. A cell someone typed
-- over since has a latest row without it and is kept.

ALTER TABLE runtime.fact_input ADD COLUMN IF NOT EXISTS link_ref UUID;
CREATE INDEX IF NOT EXISTS fact_input_link_ref_idx
    ON runtime.fact_input (link_ref, revision_id) WHERE link_ref IS NOT NULL;

ALTER TABLE runtime.fact_input_history ADD COLUMN IF NOT EXISTS link_ref UUID;

-- 133's archive function, carrying link_ref into cell history.
CREATE OR REPLACE FUNCTION runtime.archive_deleted_fact() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM core.model WHERE id = OLD.model_id)
       OR NOT EXISTS (SELECT 1 FROM model.metric_def WHERE id = OLD.metric_id) THEN
        RETURN OLD;
    END IF;
    INSERT INTO runtime.fact_input_history
        (id, model_id, revision_id, metric_id, dim_members, value, entered_by, entered_at, source_ref, delete_reason, text_value, link_ref)
    VALUES
        (OLD.id, OLD.model_id, OLD.revision_id, OLD.metric_id, OLD.dim_members, OLD.value, OLD.entered_by, OLD.entered_at, OLD.source_ref,
         COALESCE(current_setting('mvx.delete_reason', true), ''), OLD.text_value, OLD.link_ref);
    RETURN OLD;
END $$;
