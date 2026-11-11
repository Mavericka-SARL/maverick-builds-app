-- Calculated results are kept in one part per revision.
--
-- runtime.calc_result held every revision's results in one table (its range
-- partitioning by calc_at only ever had the default part). Deleting a
-- revision — an AI Developer draft, an old revision, a model or an
-- application — deleted its results one row at a time, slow for millions of
-- rows, and their space stayed in the table's files for its next rows: the
-- disk came back only when the table was rewritten (the weekly pg_repack
-- job). One Playground workspace held 2.3 GB of results (2026-11-10; owner's
-- decision: remove the cause).
--
-- The table is now partitioned by revision_id (LIST), one part per revision:
--   - inserting a revision creates its parts (trigger revision_parts_added,
--     runtime.add_revision_parts);
--   - deleting a revision, or the model or application it belongs to,
--     empties its parts with TRUNCATE before its rows would go one by one
--     (runtime.empty_revision_parts): the disk comes back at commit. A part
--     another session holds for more than three seconds is left to the
--     row-by-row delete, as before;
--   - the gateway then drops the parts of deleted revisions
--     (plan.DropGoneRevisionParts, runtime.drop_revision_part). Dropping a
--     part locks the whole table for a moment, so it waits a second at most
--     and is tried again at the next compaction.
-- There is no default part: a row of a revision without one is refused
-- ("no partition of relation found for row"), never kept where its
-- revision's delete would not reach.
--
-- The functions work on every table in schema runtime partitioned by LIST
-- (revision_id), so a table converted later takes part without a change.
--
-- The rows are copied into the new parts: on the Playground's largest
-- workspace (4.8 million rows) a few minutes, while that workspace's
-- results wait. Rows of a revision that no longer exists are not copied.
-- The primary key becomes (id, revision_id): a partitioned table's unique
-- keys include its partition key, and nothing refers to a result by key.

-- The tables partitioned by revision.
CREATE OR REPLACE FUNCTION runtime.revision_partitioned_tables()
RETURNS SETOF regclass
LANGUAGE sql STABLE
AS $$
    SELECT pt.partrelid::regclass
    FROM pg_catalog.pg_partitioned_table pt
    JOIN pg_catalog.pg_class c ON c.oid = pt.partrelid
    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
    JOIN pg_catalog.pg_attribute a ON a.attrelid = pt.partrelid AND a.attnum = pt.partattrs[0]
    WHERE n.nspname = 'runtime' AND pt.partstrat = 'l' AND pt.partnatts = 1
      AND a.attname = 'revision_id'
$$;

-- A revision's part of a table: the table's name and the revision's id
-- without dashes (at most 51 characters for the longest runtime table).
CREATE OR REPLACE FUNCTION runtime.revision_part_name(parent regclass, rev uuid)
RETURNS text
LANGUAGE sql STABLE
AS $$
    SELECT c.relname || '_' || replace(rev::text, '-', '')
    FROM pg_catalog.pg_class c WHERE c.oid = parent
$$;

-- Creates a revision's missing parts. Each is created on its own and then
-- attached: CREATE TABLE ... PARTITION OF would lock the whole table
-- (ACCESS EXCLUSIVE) until the revision's transaction commits, while ATTACH
-- takes a lock reads and writes pass (SHARE UPDATE EXCLUSIVE). Attaching
-- adds the table's foreign keys to the part, which holds writes to the
-- tables they point to (models, metrics) until that commit.
-- SECURITY DEFINER: the tables' owner attaches, whoever creates the revision.
CREATE OR REPLACE FUNCTION runtime.add_revision_parts(rev uuid)
RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    parent regclass;
    part text;
BEGIN
    FOR parent IN SELECT t FROM runtime.revision_partitioned_tables() t LOOP
        part := runtime.revision_part_name(parent, rev);
        CONTINUE WHEN to_regclass(format('runtime.%I', part)) IS NOT NULL;
        EXECUTE format('CREATE TABLE runtime.%I (LIKE %s INCLUDING DEFAULTS INCLUDING STORAGE)', part, parent);
        EXECUTE format('ALTER TABLE %s ATTACH PARTITION runtime.%I FOR VALUES IN (%L)', parent, part, rev);
    END LOOP;
END $$;

-- Empties a revision's parts, the moment before the revision is deleted.
-- TRUNCATE locks only the part, and waits for the sessions using it: three
-- seconds at most, after which the part is left to the row-by-row delete of
-- its revision's metrics (and dropped all the same once the revision is
-- gone). TRUNCATE fires no row triggers: an input of a deleted revision is
-- not archived, and the archive of its metrics goes with them (migration 133).
CREATE OR REPLACE FUNCTION runtime.empty_revision_parts(rev uuid)
RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    parent regclass;
    part regclass;
    wait text := current_setting('lock_timeout');
BEGIN
    FOR parent IN SELECT t FROM runtime.revision_partitioned_tables() t LOOP
        part := to_regclass(format('runtime.%I', runtime.revision_part_name(parent, rev)));
        CONTINUE WHEN part IS NULL;
        PERFORM set_config('lock_timeout', '3s', true);
        BEGIN
            EXECUTE format('TRUNCATE %s', part);
        EXCEPTION WHEN lock_not_available THEN
            RAISE NOTICE '% is in use: its rows are deleted one by one with its revision', part;
        END;
        PERFORM set_config('lock_timeout', wait, true);
    END LOOP;
END $$;

-- The parts whose revision no longer exists. Only a part holding exactly
-- one revision qualifies: never a default part, nor one made by hand.
CREATE OR REPLACE FUNCTION runtime.gone_revision_parts()
RETURNS TABLE (part regclass, revision_id uuid)
LANGUAGE sql STABLE
AS $$
    SELECT i.inhrelid::regclass, b.rev
    FROM runtime.revision_partitioned_tables() p(parent)
    JOIN pg_catalog.pg_inherits i ON i.inhparent = p.parent
    JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
    CROSS JOIN LATERAL (
        SELECT (regexp_match(pg_catalog.pg_get_expr(c.relpartbound, c.oid),
                '^FOR VALUES IN \(''([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})''\)$'))[1]::uuid AS rev
    ) b
    WHERE b.rev IS NOT NULL
      AND NOT EXISTS (SELECT 1 FROM model.revision r WHERE r.id = b.rev)
$$;

-- Drops one part of a deleted revision. Dropping a part locks the whole
-- table (ACCESS EXCLUSIVE) for the moment it takes, and every query of the
-- table queues behind it while it waits: one second at most, after which
-- the part is left for the next try. Returns whether it was dropped.
CREATE OR REPLACE FUNCTION runtime.drop_revision_part(part regclass)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp
AS $$
DECLARE
    wait text := current_setting('lock_timeout');
BEGIN
    IF part IS NULL OR NOT EXISTS (SELECT 1 FROM runtime.gone_revision_parts() g WHERE g.part = drop_revision_part.part) THEN
        RETURN false;
    END IF;
    PERFORM set_config('lock_timeout', '1s', true);
    BEGIN
        EXECUTE format('DROP TABLE %s', part);
    EXCEPTION WHEN lock_not_available OR undefined_table THEN
        PERFORM set_config('lock_timeout', wait, true);
        RETURN false;
    END;
    PERFORM set_config('lock_timeout', wait, true);
    RETURN true;
END $$;

CREATE OR REPLACE FUNCTION runtime.revision_parts_added()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM runtime.add_revision_parts(NEW.id);
    RETURN NULL;
END $$;

CREATE OR REPLACE FUNCTION runtime.revision_parts_emptied()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM runtime.empty_revision_parts(OLD.id);
    RETURN OLD;
END $$;

-- A model's delete reaches its results through two cascades — its
-- revisions', and the results' own model_id — in an order Postgres picks;
-- emptying every revision's parts before the model row goes makes both
-- find nothing to delete. An application's delete cascades to its models.
CREATE OR REPLACE FUNCTION runtime.model_revision_parts_emptied()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    PERFORM runtime.empty_revision_parts(r.id) FROM model.revision r WHERE r.model_id = OLD.id;
    RETURN OLD;
END $$;

DROP TRIGGER IF EXISTS revision_parts_added ON model.revision;
CREATE TRIGGER revision_parts_added AFTER INSERT ON model.revision
    FOR EACH ROW EXECUTE FUNCTION runtime.revision_parts_added();
DROP TRIGGER IF EXISTS revision_parts_emptied ON model.revision;
CREATE TRIGGER revision_parts_emptied BEFORE DELETE ON model.revision
    FOR EACH ROW EXECUTE FUNCTION runtime.revision_parts_emptied();
DROP TRIGGER IF EXISTS model_revision_parts_emptied ON core.model;
CREATE TRIGGER model_revision_parts_emptied BEFORE DELETE ON core.model
    FOR EACH ROW EXECUTE FUNCTION runtime.model_revision_parts_emptied();

-- The table, rebuilt. The old one keeps its rows until they are copied.
ALTER TABLE runtime.calc_result RENAME TO calc_result_by_time;
ALTER INDEX IF EXISTS runtime.calc_result_pkey RENAME TO calc_result_by_time_pkey;
ALTER INDEX IF EXISTS runtime.calc_result_combo_idx RENAME TO calc_result_by_time_combo_idx;
ALTER INDEX IF EXISTS runtime.calc_result_metric_idx RENAME TO calc_result_by_time_metric_idx;
ALTER INDEX IF EXISTS runtime.calc_result_revision_idx RENAME TO calc_result_by_time_revision_idx;

CREATE TABLE runtime.calc_result (
    id            uuid NOT NULL DEFAULT gen_random_uuid(),
    model_id      uuid NOT NULL,
    revision_name text,
    dim_members   jsonb NOT NULL,
    metric_id     uuid NOT NULL,
    value         numeric,
    partition_key text,
    calc_at       timestamptz NOT NULL DEFAULT now(),
    revision_id   uuid NOT NULL,
    text_value    text
) PARTITION BY LIST (revision_id);

-- Whoever could read and write the old table can the new one.
DO $$
DECLARE
    g record;
BEGIN
    FOR g IN
        SELECT a.privilege_type, a.grantee
        FROM pg_class c, aclexplode(c.relacl) a
        WHERE c.oid = 'runtime.calc_result_by_time'::regclass AND a.grantee <> c.relowner
    LOOP
        EXECUTE format('GRANT %s ON runtime.calc_result TO %s', g.privilege_type,
            CASE WHEN g.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(pg_get_userbyid(g.grantee)) END);
    END LOOP;
END $$;

SELECT runtime.add_revision_parts(id) FROM model.revision;

-- Copied before the keys and indexes exist: built once over the copied
-- rows, not row by row. One worker: parallel workers share their sort
-- through /dev/shm, which a container may have at 64 MB.
SELECT set_config('maintenance_work_mem', '256MB', true),
       set_config('max_parallel_maintenance_workers', '0', true);

INSERT INTO runtime.calc_result
    (id, model_id, revision_name, dim_members, metric_id, value, partition_key, calc_at, revision_id, text_value)
SELECT c.id, c.model_id, c.revision_name, c.dim_members, c.metric_id, c.value, c.partition_key, c.calc_at, c.revision_id, c.text_value
FROM runtime.calc_result_by_time c
WHERE EXISTS (SELECT 1 FROM model.revision r WHERE r.id = c.revision_id);

ALTER TABLE runtime.calc_result ADD CONSTRAINT calc_result_pkey PRIMARY KEY (id, revision_id);
CREATE INDEX calc_result_combo_idx ON runtime.calc_result (model_id, revision_id, metric_id, dim_members, calc_at DESC);
CREATE INDEX calc_result_metric_idx ON runtime.calc_result (metric_id);
ALTER TABLE runtime.calc_result
    ADD CONSTRAINT calc_result_model_id_fkey FOREIGN KEY (model_id) REFERENCES core.model(id) ON DELETE CASCADE;
ALTER TABLE runtime.calc_result
    ADD CONSTRAINT calc_result_metric_id_fkey FOREIGN KEY (metric_id) REFERENCES model.metric_def(id) ON DELETE CASCADE;

DROP TABLE runtime.calc_result_by_time;

ANALYZE runtime.calc_result;
