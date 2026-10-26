-- A connector's name is unique within its revision (a revision-less one,
-- within its model). Revision copies, the dashboard's connector buttons and
-- model export find a connector's copy by name: with two of one name a new
-- revision failed ("more than one row returned by a subquery"), and an
-- export dropped the reference (decided 2026-10-06).
--
-- Existing duplicates keep their oldest connector's name; each later one is
-- renamed "Name (2)", "Name (3)", … — the first such name still free.
DO $$
DECLARE
    r         record;
    n         int;
    candidate text;
BEGIN
    FOR r IN
        SELECT d.id, d.model_id, d.revision_id, d.name
        FROM (
            SELECT id, model_id, revision_id, name,
                   row_number() OVER (PARTITION BY model_id, revision_id, name ORDER BY created_at, id) AS rn
            FROM model.integration_def
        ) d
        WHERE d.rn > 1
        ORDER BY d.model_id, d.revision_id, d.name, d.rn
    LOOP
        n := 2;
        LOOP
            candidate := r.name || ' (' || n || ')';
            EXIT WHEN NOT EXISTS (
                SELECT 1 FROM model.integration_def x
                WHERE x.model_id = r.model_id
                  AND x.revision_id IS NOT DISTINCT FROM r.revision_id
                  AND x.name = candidate);
            n := n + 1;
        END LOOP;
        UPDATE model.integration_def SET name = candidate WHERE id = r.id;
    END LOOP;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS integration_def_revision_name_uq
    ON model.integration_def (model_id, revision_id, name) NULLS NOT DISTINCT;
