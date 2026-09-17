-- Finish what migration 037 started ("Eliminate scenario+version text
-- scoping. revision_id UUID is the sole scope key."): model.version was a
-- parallel, same-shape sibling of model.scenario created in the very first
-- schema migration (004); scenario won and became model.revision (045),
-- version never grew any real functionality beyond a display label nothing
-- actually reads (the "VERSION" badge was hardcoded to the literal string
-- "draft", never wired to active_version) and a management endpoint with no
-- callers. revision_id is the sole scope key everywhere; drop the rest.

DROP TABLE IF EXISTS model.version;

ALTER TABLE core.model DROP COLUMN IF EXISTS active_version;

-- CASCADE: internal/schemamigration generates disposable, regenerable
-- per-model BI views (runtime.m_<id>_inputs/_results) that may reference
-- these columns; dropping them along with the column is correct — the
-- generator no longer emits a version reference, and the view itself is
-- meant to be regenerated on demand, not hand-maintained data.
ALTER TABLE runtime.fact_input  DROP COLUMN IF EXISTS version CASCADE;
ALTER TABLE runtime.calc_result DROP COLUMN IF EXISTS version CASCADE;

ALTER TABLE runtime.metric_partition_state
    DROP COLUMN IF EXISTS scenario,
    DROP COLUMN IF EXISTS version;

ALTER TABLE import.import_job
    DROP COLUMN IF EXISTS scenario,
    DROP COLUMN IF EXISTS version;
