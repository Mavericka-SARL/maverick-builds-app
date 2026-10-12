-- A storage limit refuses the write that would cross it, and deleted data
-- gives its disk back.
--
-- Until now a Playground workspace was checked against its edition's
-- storage limit only before bulk writes (plan.CheckStorage: refused once
-- already over) and by the five-minute sweep, which turned it read-only
-- after the fact: an import could take it from 90 MB to 300 MB. Now every
-- statement that adds rows to the tables a workspace fills is checked as
-- it runs, and refused — the whole transaction — when it would cross the
-- limit. And PostgreSQL keeps a deleted row's space in a table's files for
-- later writes; the usage sweep now rewrites tables that hold enough of it
-- (plan.Compact), remembering what it has rewritten here.

-- runtime.live_tables is every ordinary table of the database (catalogs
-- excluded) with its root (the partitioned table a partition belongs to,
-- else itself), its live rows, the bytes they take and the bytes its files
-- take:
--   - live rows from the statistics collector, or the planner's count when
--     the collector has nothing (just after a restart);
--   - a row's width as the planner statistics give it: the columns' average
--     stored widths, a 24-byte header, alignment, and a 4-byte line pointer;
--   - the table's indexes and TOAST in the proportion its live rows take of
--     its main file, as they hold the same rows;
--   - a table the planner has not analysed yet at its files' size.
-- plan.StorageBytes and the guard below both read it.
CREATE OR REPLACE FUNCTION runtime.live_tables()
RETURNS TABLE (relid oid, root oid, live bigint, bytes numeric, files bigint)
LANGUAGE sql STABLE AS $$
    WITH stat AS (
        SELECT schemaname, tablename, sum(avg_width * (1 - null_frac)) AS width
        FROM pg_stats WHERE NOT inherited GROUP BY schemaname, tablename),
    tbl AS (
        SELECT c.oid, COALESCE(pg_partition_root(c.oid), c.oid) AS root,
               CASE WHEN s.n_live_tup + s.n_dead_tup + s.n_tup_ins + s.n_tup_del > 0 THEN s.n_live_tup
                    ELSE GREATEST(c.reltuples, 0)::bigint END AS live,
               pg_relation_size(c.oid) AS heap,
               pg_total_relation_size(c.oid) - pg_relation_size(c.oid) AS rest,
               ceil((24 + st.width) / 8.0) * 8 + 4 AS row_bytes
        FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        LEFT JOIN pg_stat_user_tables s ON s.relid = c.oid
        LEFT JOIN stat st ON st.schemaname = n.nspname AND st.tablename = c.relname
        WHERE c.relkind IN ('r', 'm') AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'pg_toast'))
    SELECT oid, root, live,
           CASE WHEN live = 0 THEN 0
                WHEN row_bytes IS NULL OR heap = 0 THEN heap + rest
                ELSE live * row_bytes + rest * LEAST(1, live * row_bytes / heap::numeric)
           END,
           heap + rest
    FROM tbl
$$;

-- runtime.bytes_per_row is what one more row of a table (a partitioned one
-- by its root) takes, indexes included: its live data over its live rows
-- once it holds a thousand, else 300 bytes — a table of a few rows has
-- pages and index pages nearly empty, and its files over its rows would
-- price a row at kilobytes. plan.ModelStorage prices a model's rows the
-- same way.
CREATE OR REPLACE FUNCTION runtime.bytes_per_row(root_table oid) RETURNS numeric
LANGUAGE sql STABLE AS $$
    SELECT CASE WHEN sum(l.live) >= 1000 THEN sum(l.bytes) / sum(l.live) ELSE 300 END
    FROM runtime.live_tables() l WHERE l.root = root_table
$$;

-- The tenant's storage limit as its edition sets it, kept on its own row by
-- the usage sweep (plan.Sweep) for the guard to read: NULL is no limit.
-- storage_limit_text is the sentence a refusal ends with.
ALTER TABLE core.customer
    ADD COLUMN IF NOT EXISTS storage_limit_bytes BIGINT,
    ADD COLUMN IF NOT EXISTS storage_limit_text  TEXT;

-- runtime.storage_mark is each table's insert and delete counters
-- (pg_stat_user_tables) taken by the usage sweep just before it measured
-- the workspace (core.customer.storage_bytes). runtime.storage_used is that
-- measurement plus the rows inserted and deleted since, at each table's
-- bytes per row: the counters count every row exactly, where the live-row
-- estimate an ANALYZE resets can count a recent insert twice. Without a
-- measurement yet it is the live data as the statistics give it.
CREATE TABLE IF NOT EXISTS runtime.storage_mark (
    relid OID    PRIMARY KEY,
    ins   BIGINT NOT NULL,
    del   BIGINT NOT NULL
);

CREATE OR REPLACE FUNCTION runtime.storage_used() RETURNS numeric
LANGUAGE sql STABLE AS $$
    WITH measured AS (
        SELECT c.storage_bytes FROM core.customer c
        WHERE 'tenant_' || replace(c.id::text, '-', '') = current_database()),
    since AS (
        SELECT CASE WHEN s.n_tup_ins >= m.ins THEN s.n_tup_ins - m.ins ELSE s.n_tup_ins END
             - CASE WHEN s.n_tup_del >= m.del THEN s.n_tup_del - m.del ELSE s.n_tup_del END AS rows,
               CASE WHEN l.live >= 1000 THEN l.bytes / l.live ELSE 300 END AS per_row
        FROM pg_stat_user_tables s
        JOIN runtime.live_tables() l ON l.relid = s.relid
        LEFT JOIN runtime.storage_mark m ON m.relid = s.relid)
    SELECT CASE WHEN (SELECT storage_bytes FROM measured) IS NULL
                THEN (SELECT COALESCE(sum(bytes), 0) FROM runtime.live_tables())
                ELSE (SELECT storage_bytes FROM measured)
                     + (SELECT COALESCE(sum(rows * per_row) FILTER (WHERE rows <> 0), 0) FROM since)
           END
$$;

-- runtime.storage_guard refuses a statement that would take the workspace
-- past its limit. The first statement of a transaction measures the room
-- left (runtime.storage_used); each one takes its rows, at its table's bytes
-- per row, from that room, kept for the rest of the transaction in
-- mvx.storage_left. Statement-level, with the statement's rows as a
-- transition table: an import of 100,000 rows is one check. A transaction
-- that only deletes is never refused.
CREATE OR REPLACE FUNCTION runtime.storage_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    added     bigint;
    per_row   numeric;
    room      numeric := NULLIF(current_setting('mvx.storage_left', true), '')::numeric;
    lim       bigint;
    note      text;
BEGIN
    SELECT count(*) INTO added FROM new_rows;
    IF added = 0 THEN
        RETURN NULL;
    END IF;
    IF room IS NULL THEN
        SELECT c.storage_limit_bytes INTO lim FROM core.customer c
        WHERE 'tenant_' || replace(c.id::text, '-', '') = current_database();
        IF lim IS NULL THEN
            RETURN NULL;
        END IF;
        room := lim - runtime.storage_used();
    END IF;
    per_row := NULLIF(current_setting('mvx.storage_row_' || TG_RELID::text, true), '')::numeric;
    IF per_row IS NULL THEN
        per_row := runtime.bytes_per_row(TG_RELID);
        PERFORM set_config('mvx.storage_row_' || TG_RELID::text, per_row::text, true);
    END IF;
    room := room - added * per_row;
    IF room < 0 THEN
        SELECT c.storage_limit_bytes, c.storage_limit_text INTO lim, note FROM core.customer c
        WHERE 'tenant_' || replace(c.id::text, '-', '') = current_database();
        RAISE EXCEPTION USING
            ERRCODE = 'MV402',
            MESSAGE = format('Not saved: this would take the workspace to about %s MB. %s',
                             ceil((lim - room) / 1048576.0), COALESCE(note, '')),
            DETAIL  = format('limit_bytes=%s', lim);
    END IF;
    PERFORM set_config('mvx.storage_left', room::text, true);
    RETURN NULL;
END $$;

-- runtime.set_storage_guard puts the guard on the tables a workspace fills,
-- or takes it off — no cost at all for a workspace without a limit. The
-- usage sweep calls it with whether the edition limits storage; it changes
-- nothing when the guard is already as asked.
CREATE OR REPLACE FUNCTION runtime.set_storage_guard(want boolean) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    t   regclass;
    has boolean;
BEGIN
    FOREACH t IN ARRAY ARRAY['runtime.fact_input', 'import.import_staging', 'runtime.form_record', 'model.dimension_member']::regclass[] LOOP
        SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = t AND tgname = 'storage_guard' AND tgparentid = 0) INTO has;
        IF want AND NOT has THEN
            EXECUTE format('CREATE TRIGGER storage_guard AFTER INSERT ON %s REFERENCING NEW TABLE AS new_rows
                            FOR EACH STATEMENT EXECUTE FUNCTION runtime.storage_guard()', t);
        ELSIF has AND NOT want THEN
            EXECUTE format('DROP TRIGGER storage_guard ON %s', t);
        END IF;
    END LOOP;
END $$;

-- What the usage sweep's compaction rewrote, per table: the table's
-- changed-row count (deletes and non-HOT updates) when it was last
-- rewritten, so only rows changed since count towards the next one.
CREATE TABLE IF NOT EXISTS runtime.table_compaction (
    relid   OID    PRIMARY KEY,
    changed BIGINT NOT NULL
);

-- A committed import's staged rows are in its facts; nothing reads them
-- again (importpkg now deletes them at commit).
DELETE FROM import.import_staging s
USING import.import_job j
WHERE j.id = s.job_id AND j.status = 'committed';
