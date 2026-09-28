-- Access rules follow a member or metric into every revision: lineage ids.
--
-- A 'dimension_member' / 'metric' rule stores the id of one row, but every
-- revision copy re-mints row ids, so on its own a rule restricts only the
-- revision its row lives in — an old revision read unrestricted.
--
-- lineage_id is the identity a dimension, member or metric shares with its
-- copies in every revision of its model. A new row gets a fresh one; every
-- definition copy (revision duplication by the developer or the AI
-- assistant) carries it over; model export writes it into the package and
-- import keeps it (unless it is already taken by another model). A rename
-- never touches it, so a rule survives renames of the dimension, the member
-- code or the metric. A row deleted and re-added is a NEW lineage: the
-- re-added row is unrestricted until an admin sets a rule on it, while the
-- old revisions' copies of the original stay restricted.
--
-- identity.user_access_rule.ref_lineage_id is the lineage of the row ref_id
-- points at, set by every rule write in application code. It is what a
-- rule resolves by when a revision is read (writeguard.RulesForRevision),
-- and it outlives the row. NULL for rule types without a lineage
-- ('button').
--
-- Re-runnable: the DDL is guarded (IF NOT EXISTS) and the backfills are
-- no-ops once applied, so a run applied but not yet recorded in _migrations
-- (pkg/migrate records it in a separate statement) does not stop the next
-- start.

ALTER TABLE model.dimension_def    ADD COLUMN IF NOT EXISTS lineage_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE model.dimension_member ADD COLUMN IF NOT EXISTS lineage_id UUID NOT NULL DEFAULT gen_random_uuid();
ALTER TABLE model.metric_def       ADD COLUMN IF NOT EXISTS lineage_id UUID NOT NULL DEFAULT gen_random_uuid();

-- Backfill: rows sharing an identity across the revisions of a model share
-- one lineage — the best identity available before lineage existed.
-- Dimensions and metrics by name (case-insensitive), members by (dimension
-- name, code). Revision-less rows (revision_id NULL) are included. The
-- oldest row of each group lends its lineage to the rest.
WITH g AS (
    SELECT id, first_value(lineage_id) OVER (
               PARTITION BY model_id, lower(name) ORDER BY created_at, id) AS lineage
    FROM model.dimension_def
)
UPDATE model.dimension_def d SET lineage_id = g.lineage
FROM g WHERE g.id = d.id AND d.lineage_id <> g.lineage;

WITH g AS (
    SELECT m.id, first_value(m.lineage_id) OVER (
               PARTITION BY d.model_id, lower(d.name), m.code ORDER BY m.created_at, m.id) AS lineage
    FROM model.dimension_member m
    JOIN model.dimension_def d ON d.id = m.dimension_id
)
UPDATE model.dimension_member m SET lineage_id = g.lineage
FROM g WHERE g.id = m.id AND m.lineage_id <> g.lineage;

WITH g AS (
    SELECT id, first_value(lineage_id) OVER (
               PARTITION BY model_id, lower(name) ORDER BY created_at, id) AS lineage
    FROM model.metric_def
)
UPDATE model.metric_def md SET lineage_id = g.lineage
FROM g WHERE g.id = md.id AND md.lineage_id <> g.lineage;

CREATE INDEX IF NOT EXISTS dimension_def_lineage_idx    ON model.dimension_def (lineage_id);
CREATE INDEX IF NOT EXISTS dimension_member_lineage_idx ON model.dimension_member (lineage_id);
CREATE INDEX IF NOT EXISTS metric_def_lineage_idx       ON model.metric_def (lineage_id);

ALTER TABLE identity.user_access_rule ADD COLUMN IF NOT EXISTS ref_lineage_id UUID;

-- Backfill from the row each rule points at. A malformed ref_id, or one
-- whose row is already gone, stays NULL (it restricts nothing today either).
-- The CASE casts ref_id only once it is known to be a well-formed uuid.
UPDATE identity.user_access_rule r
SET ref_lineage_id = m.lineage_id
FROM model.dimension_member m
WHERE r.rule_type = 'dimension_member'
  AND m.id = CASE WHEN r.ref_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                   THEN r.ref_id::uuid END;

UPDATE identity.user_access_rule r
SET ref_lineage_id = md.lineage_id
FROM model.metric_def md
WHERE r.rule_type = 'metric'
  AND md.id = CASE WHEN r.ref_id ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
                   THEN r.ref_id::uuid END;

CREATE INDEX IF NOT EXISTS user_access_rule_lineage_idx ON identity.user_access_rule (ref_lineage_id)
    WHERE ref_lineage_id IS NOT NULL;
