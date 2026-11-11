-- A revision's parts are made on their own, never inside the transaction
-- that creates the revision.
--
-- Migration 176 attached a revision's parts from a trigger on its insert.
-- Attaching a part adds the table's foreign keys to it, which takes the
-- lock on the tables they point to (core.model, model.metric_def) that
-- holds their writes — until the creating transaction ends. A revision copy,
-- or an AI Developer draft made inside a longer transaction, held every
-- model and metric write of the workspace for its whole length, and a
-- request that then waited on its own second connection stood still until
-- its deadline (the gateway's AI Developer tests, 90 s, 2026-11-10).
--
-- Now:
--   - a default part takes the results of a revision that has no part yet;
--   - runtime.add_revision_parts makes a revision's parts on its own
--     (called outside any transaction): it moves the revision's rows out of
--     the default part and attaches, waiting two seconds at most for its
--     locks, and skips a revision another transaction is still creating.
--     The calculation calls it before a revision's pass, and every
--     compaction for each revision still without its parts
--     (runtime.revisions_without_parts, plan.EnsureRevisionParts);
--   - a revision's delete empties and the compaction drops its parts, as
--     in 176; its rows still in the default part go row by row.

DROP TRIGGER IF EXISTS revision_parts_added ON model.revision;
DROP FUNCTION IF EXISTS runtime.revision_parts_added();
DROP FUNCTION IF EXISTS runtime.add_revision_parts(uuid);

-- Returns whether the revision has all its parts afterwards.
CREATE FUNCTION runtime.add_revision_parts(rev uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    parent regclass;
    ref regclass;
    part text;
    deflt regclass;
    wait text := current_setting('lock_timeout');
    done boolean := true;
BEGIN
    -- A revision another transaction is still creating is not visible yet.
    IF NOT EXISTS (SELECT 1 FROM model.revision r WHERE r.id = rev) THEN
        RETURN false;
    END IF;
    FOR parent IN SELECT t FROM runtime.revision_partitioned_tables() t LOOP
        part := runtime.revision_part_name(parent, rev);
        CONTINUE WHEN to_regclass(format('runtime.%I', part)) IS NOT NULL;
        deflt := NULL;
        SELECT i.inhrelid::regclass INTO deflt
        FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
        WHERE i.inhparent = parent AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT';
        PERFORM set_config('lock_timeout', '2s', true);
        BEGIN
            -- Locks in the order a delete takes them: the tables the
            -- foreign keys point to (models, metrics), then the table, then
            -- its default part. A model's delete holds the model's row and
            -- then deletes its results from the default part; taking the
            -- default part first (as ATTACH alone does, before the models)
            -- deadlocked with it.
            FOR ref IN SELECT DISTINCT c.confrelid::regclass FROM pg_constraint c
                       WHERE c.conrelid = parent AND c.contype = 'f' ORDER BY 1 LOOP
                EXECUTE format('LOCK TABLE %s IN SHARE ROW EXCLUSIVE MODE', ref);
            END LOOP;
            EXECUTE format('LOCK TABLE %s IN SHARE UPDATE EXCLUSIVE MODE', parent);
            IF deflt IS NOT NULL THEN
                EXECUTE format('LOCK TABLE %s IN ACCESS EXCLUSIVE MODE', deflt);
            END IF;
            -- Deleted while this waited: nothing to make.
            IF EXISTS (SELECT 1 FROM model.revision r WHERE r.id = rev)
               AND to_regclass(format('runtime.%I', part)) IS NULL THEN
                EXECUTE format('CREATE TABLE runtime.%I (LIKE %s INCLUDING DEFAULTS INCLUDING STORAGE)', part, parent);
                IF deflt IS NOT NULL THEN
                    EXECUTE format('WITH moved AS (DELETE FROM %s WHERE revision_id = %L RETURNING *) INSERT INTO runtime.%I SELECT * FROM moved',
                        deflt, rev, part);
                END IF;
                EXECUTE format('ALTER TABLE %s ATTACH PARTITION runtime.%I FOR VALUES IN (%L)', parent, part, rev);
            END IF;
        EXCEPTION
            WHEN lock_not_available OR deadlock_detected THEN
                done := false;
            -- Another session made it meanwhile.
            WHEN duplicate_table OR duplicate_object THEN
                NULL;
        END;
        PERFORM set_config('lock_timeout', wait, true);
    END LOOP;
    RETURN done;
END $$;

-- The revisions missing a part of any table kept by revision.
CREATE OR REPLACE FUNCTION runtime.revisions_without_parts()
RETURNS SETOF uuid
LANGUAGE sql STABLE
AS $$
    SELECT r.id FROM model.revision r
    WHERE EXISTS (
        SELECT 1 FROM runtime.revision_partitioned_tables() p(parent)
        WHERE to_regclass(format('runtime.%I', runtime.revision_part_name(p.parent, r.id))) IS NULL)
$$;

CREATE TABLE IF NOT EXISTS runtime.calc_result_default PARTITION OF runtime.calc_result DEFAULT;
