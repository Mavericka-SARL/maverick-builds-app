-- Per-cell change history (enterprise feature `cell_history`).
--
-- runtime.fact_input is append-only: every write is a new row and readers
-- take the latest (entered_at DESC, id DESC), so the history of an
-- intersection is already there — with one hole. Three paths DELETE rows
-- (an import in full-reload mode, a form mapping re-posting its aggregates,
-- re-parenting a member re-keys its facts), and a deleted row is a change
-- that vanished from the record. This archive keeps it: a row trigger copies
-- every deleted fact here before it goes, with the reason the deleting
-- transaction declared through SET LOCAL mvx.delete_reason. No foreign keys
-- on purpose — a metric or model being deleted must not fail because its
-- history exists, and history of something deleted is still history.

CREATE TABLE IF NOT EXISTS runtime.fact_input_history (
    id            UUID        NOT NULL,          -- the original fact_input id
    model_id      UUID        NOT NULL,
    revision_id   UUID,
    metric_id     UUID        NOT NULL,
    dim_members   JSONB       NOT NULL,
    value         NUMERIC     NOT NULL,
    entered_by    UUID,
    entered_at    TIMESTAMPTZ NOT NULL,
    source_ref    UUID,
    deleted_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    delete_reason TEXT        NOT NULL DEFAULT '',
    PRIMARY KEY (id, deleted_at)
);
CREATE INDEX IF NOT EXISTS fact_input_history_cell_idx
    ON runtime.fact_input_history (model_id, revision_id, metric_id, entered_at DESC);
CREATE INDEX IF NOT EXISTS fact_input_history_dims_idx
    ON runtime.fact_input_history USING GIN (dim_members);

CREATE OR REPLACE FUNCTION runtime.archive_deleted_fact() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    INSERT INTO runtime.fact_input_history
        (id, model_id, revision_id, metric_id, dim_members, value, entered_by, entered_at, source_ref, delete_reason)
    VALUES
        (OLD.id, OLD.model_id, OLD.revision_id, OLD.metric_id, OLD.dim_members, OLD.value, OLD.entered_by, OLD.entered_at, OLD.source_ref,
         COALESCE(current_setting('mvx.delete_reason', true), ''));
    RETURN OLD;
END $$;

DROP TRIGGER IF EXISTS fact_input_archive_on_delete ON runtime.fact_input;
CREATE TRIGGER fact_input_archive_on_delete
    BEFORE DELETE ON runtime.fact_input
    FOR EACH ROW EXECUTE FUNCTION runtime.archive_deleted_fact();
