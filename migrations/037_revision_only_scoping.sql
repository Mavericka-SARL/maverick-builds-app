-- Eliminate scenario+version text scoping. revision_id UUID is the sole scope key.
--
-- 1. Make revision_id NOT NULL in runtime tables (backfill from scenario name match,
--    fallback to model's active_revision_id).
-- 2. Make scenario and version nullable (no longer used as scope keys).
-- 3. Update import.import_job similarly.
-- 4. Update runtime.form_record_posting to use revision_id.
-- 5. Remove scenario/version columns from model.form_metric_mapping.

-- ─── runtime.fact_input ──────────────────────────────────────────────────────

-- Backfill any remaining NULL revision_ids using scenario name match.
-- Note: target table alias (fi) cannot appear in FROM-clause JOIN conditions;
-- use WHERE instead.
UPDATE runtime.fact_input fi
SET revision_id = s.id
FROM model.scenario s
WHERE fi.revision_id IS NULL
  AND s.model_id = fi.model_id
  AND s.name = fi.scenario;

-- Fallback: link to model's active revision for any still-null rows.
UPDATE runtime.fact_input fi
SET revision_id = m.active_revision_id
FROM core.model m
WHERE fi.revision_id IS NULL
  AND m.id = fi.model_id
  AND m.active_revision_id IS NOT NULL;

-- Fallback: link to most-recent revision for any remaining null rows.
UPDATE runtime.fact_input fi
SET revision_id = (
    SELECT s.id FROM model.scenario s WHERE s.model_id = fi.model_id ORDER BY s.created_at DESC LIMIT 1
)
WHERE fi.revision_id IS NULL;

ALTER TABLE runtime.fact_input
    ALTER COLUMN revision_id SET NOT NULL,
    ALTER COLUMN scenario DROP NOT NULL,
    ALTER COLUMN version  DROP NOT NULL;

-- ─── runtime.calc_result ─────────────────────────────────────────────────────

UPDATE runtime.calc_result cr
SET revision_id = s.id
FROM model.scenario s
WHERE cr.revision_id IS NULL
  AND s.model_id = cr.model_id
  AND s.name = cr.scenario;

UPDATE runtime.calc_result cr
SET revision_id = m.active_revision_id
FROM core.model m
WHERE cr.revision_id IS NULL
  AND m.id = cr.model_id
  AND m.active_revision_id IS NOT NULL;

UPDATE runtime.calc_result cr
SET revision_id = (
    SELECT s.id FROM model.scenario s WHERE s.model_id = cr.model_id ORDER BY s.created_at DESC LIMIT 1
)
WHERE cr.revision_id IS NULL;

ALTER TABLE runtime.calc_result
    ALTER COLUMN revision_id SET NOT NULL,
    ALTER COLUMN scenario DROP NOT NULL,
    ALTER COLUMN version  DROP NOT NULL;

-- ─── runtime.metric_partition_state ──────────────────────────────────────────

ALTER TABLE runtime.metric_partition_state
    ALTER COLUMN scenario DROP NOT NULL,
    ALTER COLUMN version  DROP NOT NULL;

-- ─── import.import_job ───────────────────────────────────────────────────────

ALTER TABLE import.import_job
    ADD COLUMN IF NOT EXISTS revision_id UUID,
    ALTER COLUMN scenario DROP NOT NULL,
    ALTER COLUMN version  DROP NOT NULL;

-- Backfill revision_id for existing import jobs.
UPDATE import.import_job ij
SET revision_id = s.id
FROM model.scenario s
WHERE ij.revision_id IS NULL
  AND s.model_id = ij.model_id
  AND s.name = ij.scenario;

-- ─── runtime.form_record_posting ─────────────────────────────────────────────

ALTER TABLE runtime.form_record_posting
    ADD COLUMN IF NOT EXISTS revision_id UUID;

-- Backfill from scenario name match.
-- Move fp.scenario condition to WHERE — target alias cannot appear in JOIN ON.
UPDATE runtime.form_record_posting fp
SET revision_id = s.id
FROM model.form_metric_mapping m
JOIN model.scenario s ON s.model_id = m.model_id
WHERE fp.revision_id IS NULL
  AND m.id = fp.mapping_id
  AND s.name = fp.scenario;

-- Fallback: model's active revision.
UPDATE runtime.form_record_posting fp
SET revision_id = mo.active_revision_id
FROM model.form_metric_mapping m
JOIN core.model mo ON mo.id = m.model_id
WHERE fp.revision_id IS NULL
  AND m.id = fp.mapping_id
  AND mo.active_revision_id IS NOT NULL;

-- Fallback: most-recent revision.
UPDATE runtime.form_record_posting fp
SET revision_id = (
    SELECT s.id FROM model.form_metric_mapping m2
    JOIN model.scenario s ON s.model_id = m2.model_id
    WHERE m2.id = fp.mapping_id
    ORDER BY s.created_at DESC LIMIT 1
)
WHERE fp.revision_id IS NULL;

-- Now drop scenario/version from form_record_posting.
ALTER TABLE runtime.form_record_posting
    DROP COLUMN IF EXISTS scenario,
    DROP COLUMN IF EXISTS version;

-- ─── model.form_metric_mapping ───────────────────────────────────────────────

ALTER TABLE model.form_metric_mapping
    ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.scenario(id) ON DELETE SET NULL,
    DROP COLUMN IF EXISTS scenario_source,
    DROP COLUMN IF EXISTS scenario_field,
    DROP COLUMN IF EXISTS scenario_value,
    DROP COLUMN IF EXISTS version_source,
    DROP COLUMN IF EXISTS version_field,
    DROP COLUMN IF EXISTS version_value;
