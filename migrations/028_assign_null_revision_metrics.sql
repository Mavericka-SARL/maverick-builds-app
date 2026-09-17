-- Assign pre-existing NULL-revision metric/dimension definitions to the first
-- scenario (revision) for each model. These definitions existed before revision
-- isolation was introduced (migration 027) and would otherwise be invisible
-- to developers working inside any specific revision.
-- We pick the earliest scenario (created_at ASC) as the canonical owner.

UPDATE model.metric_def md
SET revision_id = (
    SELECT s.id FROM model.scenario s
    WHERE s.model_id = md.model_id
    ORDER BY s.created_at ASC
    LIMIT 1
)
WHERE md.revision_id IS NULL
  AND EXISTS (SELECT 1 FROM model.scenario s WHERE s.model_id = md.model_id);

UPDATE model.dimension_def dd
SET revision_id = (
    SELECT s.id FROM model.scenario s
    WHERE s.model_id = dd.model_id
    ORDER BY s.created_at ASC
    LIMIT 1
)
WHERE dd.revision_id IS NULL
  AND EXISTS (SELECT 1 FROM model.scenario s WHERE s.model_id = dd.model_id);
