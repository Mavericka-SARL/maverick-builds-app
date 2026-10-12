-- Cell history of a deleted model, revision or metric goes with it, and each
-- model's storage is recorded.
--
-- 089 stopped the archive trigger copying the facts of a model being deleted
-- (the cascade runs after the model row is gone) and dropped the model's
-- earlier history. 111 re-created the function for text_value without that
-- check, so since then deleting a model copied every one of its facts into
-- runtime.fact_input_history, and 089's model trigger deleted them again: a
-- model's whole size written twice more, as dead rows. Deleting a revision or
-- a metric was worse — the facts go by the metric_def cascade (a revision's
-- metrics go with it), and were archived for good under a metric no reader
-- can name again (ee/cellhistory reads by metric and revision), so deleting
-- them made the workspace bigger.
--
-- Now the facts of a model or metric being deleted are not archived, and a
-- deleted metric's earlier history goes too, as a model's does.

CREATE OR REPLACE FUNCTION runtime.archive_deleted_fact() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM core.model WHERE id = OLD.model_id)
       OR NOT EXISTS (SELECT 1 FROM model.metric_def WHERE id = OLD.metric_id) THEN
        RETURN OLD;
    END IF;
    INSERT INTO runtime.fact_input_history
        (id, model_id, revision_id, metric_id, dim_members, value, entered_by, entered_at, source_ref, delete_reason, text_value)
    VALUES
        (OLD.id, OLD.model_id, OLD.revision_id, OLD.metric_id, OLD.dim_members, OLD.value, OLD.entered_by, OLD.entered_at, OLD.source_ref,
         COALESCE(current_setting('mvx.delete_reason', true), ''), OLD.text_value);
    RETURN OLD;
END $$;

CREATE OR REPLACE FUNCTION runtime.drop_history_of_deleted_metric() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    DELETE FROM runtime.fact_input_history WHERE model_id = OLD.model_id AND metric_id = OLD.id;
    RETURN OLD;
END $$;

DROP TRIGGER IF EXISTS metric_drop_history_on_delete ON model.metric_def;
CREATE TRIGGER metric_drop_history_on_delete
    AFTER DELETE ON model.metric_def
    FOR EACH ROW EXECUTE FUNCTION runtime.drop_history_of_deleted_metric();

-- What the hole left behind.
DELETE FROM runtime.fact_input_history h
WHERE NOT EXISTS (SELECT 1 FROM core.model m WHERE m.id = h.model_id)
   OR NOT EXISTS (SELECT 1 FROM model.metric_def d WHERE d.id = h.metric_id);

-- Each model's share of the workspace's storage, as the edition-limits sweep
-- last measured it (internal/plan), so the tenant admin sees which model
-- holds the space. Its own table, so a measurement leaves the model row
-- (updated_at) alone.
CREATE TABLE IF NOT EXISTS runtime.model_storage (
    model_id UUID   PRIMARY KEY REFERENCES core.model(id) ON DELETE CASCADE,
    bytes    BIGINT NOT NULL
);
