-- A model's data generation: a number that grows with every committed
-- transaction that changed what a grid read of the model can answer — its
-- values, results, definitions, records, workflow state — or something every
-- model's reads depend on (users, roles, access rules, the workspace). A
-- dashboard's widgets poll their reads every two seconds, and a read pinning
-- two dimensions works its answer out again each time, though nothing
-- changed (an OPEX budget's Management summary: eight 1–1.6 s reads every
-- 2 s, 2026-11-22). With the generation in a read's ETag
-- (internal/gateway/read_etag.go), an unchanged poll is answered 304 Not
-- Modified without being worked out. Per model, so that a save in one model
-- leaves every other model's dashboards — of this workspace, or of the other
-- workspaces a shared database holds — answering 304.
--
-- Each writing transaction adds one row per model it changed to
-- runtime.data_change, visible when it commits — never before, as a
-- sequence's nextval is: a reader that took the number before a writer
-- committed would otherwise keep the old answer under the new number. A
-- change that belongs to no one model is noted under the nil UUID, which
-- every model's generation counts. A model's generation is its rows and the
-- nil UUID's, counted, plus those pruned (runtime.prune_data_changes moves
-- them into pruned in one statement), so it only ever grows with commits.
--
-- How a changed row names its model (watch_data_changes' model_of):
--   'model'    its model_id column;
--   'self'     its id (core.model);
--   'revision' its revision's model;
--   'parent'   via_sql run with its via_column (a member's dimension, a
--              widget's dashboard, a record's table, a step's workflow);
--   'database' none — every model's reads depend on it (users, roles,
--              access rules, the workspace).
-- A row whose model cannot be found (its parent deleted in the same
-- transaction) is noted under the nil UUID: coarser, never stale.
--
-- The large tables (calc_result, fact_input, ...) are noted once per
-- statement from the rows it changed (transition tables); the others per
-- row, where an UPDATE that changes nothing — or only a bookkeeping column
-- (updated_at, which core.set_updated_at stamps on every update;
-- last_seen_at, next_fire_at, reminded_at, and the edition sweep's
-- usage_checked_at and storage_bytes every five minutes) — is not a change.
-- Tables a read never consults, or that background loops write on a clock
-- (calc_engine_state's claim, the storage figures, integration claims), are
-- left out: noted, they would make every poll a miss. A table added later is
-- watched with runtime.watch_data_changes; TestEveryValueTableIsWatched
-- lists the tables of these schemas that are neither watched nor left out
-- on purpose, and TestDataChangeModelLookupsRun runs every parent lookup.

CREATE TABLE IF NOT EXISTS runtime.data_change (
    model_id uuid NOT NULL, -- the nil UUID: every model
    txid     xid8 NOT NULL,
    PRIMARY KEY (model_id, txid)
);

CREATE TABLE IF NOT EXISTS runtime.data_generation (
    model_id uuid PRIMARY KEY,
    pruned   bigint NOT NULL DEFAULT 0
);

-- note_data_changes records the current transaction once for each model:
-- the setting remembers which it did (local to the transaction, and undone
-- with a rolled-back savepoint together with the rows). A model deleted in
-- the transaction is not noted: no one reads it any more, and deleting a
-- model leaves no row of it (forget_data_changes).
CREATE OR REPLACE FUNCTION runtime.note_data_changes(models uuid[]) RETURNS void
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    tx text := pg_current_xact_id()::text;
    noted text := current_setting('mavericks.data_change', true);
    m uuid;
    added boolean := false;
BEGIN
    IF noted IS NULL OR split_part(noted, '|', 1) IS DISTINCT FROM tx THEN
        noted := tx || '|';
        added := true;
    END IF;
    FOREACH m IN ARRAY coalesce(models, '{}') LOOP
        m := coalesce(m, '00000000-0000-0000-0000-000000000000');
        IF position(m::text IN noted) = 0 THEN
            IF m = '00000000-0000-0000-0000-000000000000' OR EXISTS (SELECT 1 FROM core.model WHERE id = m) THEN
                INSERT INTO runtime.data_change (model_id, txid) VALUES (m, pg_current_xact_id()) ON CONFLICT DO NOTHING;
            END IF;
            noted := noted || m::text || ',';
            added := true;
        END IF;
    END LOOP;
    IF added THEN
        PERFORM set_config('mavericks.data_change', noted, true);
    END IF;
END $$;

-- forget_data_changes removes a deleted model's notes and its pruned count:
-- a deleted model leaves no row of it behind.
CREATE OR REPLACE FUNCTION runtime.forget_data_changes() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
BEGIN
    DELETE FROM runtime.data_change WHERE model_id = OLD.id;
    DELETE FROM runtime.data_generation WHERE model_id = OLD.id;
    RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS forget_data_changes ON core.model;
CREATE TRIGGER forget_data_changes AFTER DELETE ON core.model
    FOR EACH ROW EXECUTE FUNCTION runtime.forget_data_changes();

-- note_row_change: a row trigger's model (TG_ARGV: model_of, via_column,
-- via_sql), for the row before and after.
CREATE OR REPLACE FUNCTION runtime.note_row_change() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    versions jsonb[] := '{}';
    r jsonb;
    m uuid;
    models uuid[] := '{}';
BEGIN
    IF TG_OP = 'TRUNCATE' THEN
        PERFORM runtime.note_data_changes(ARRAY[NULL::uuid]);
        RETURN NULL;
    END IF;
    IF TG_OP <> 'INSERT' THEN versions := versions || to_jsonb(OLD); END IF;
    IF TG_OP <> 'DELETE' THEN versions := versions || to_jsonb(NEW); END IF;
    FOREACH r IN ARRAY versions LOOP
        m := NULL;
        CASE TG_ARGV[0]
            WHEN 'model' THEN m := (r ->> 'model_id')::uuid;
            WHEN 'self' THEN m := (r ->> 'id')::uuid;
            WHEN 'revision' THEN
                SELECT rv.model_id INTO m FROM model.revision rv WHERE rv.id = (r ->> 'revision_id')::uuid;
            WHEN 'parent' THEN
                IF r ->> TG_ARGV[1] IS NOT NULL THEN
                    EXECUTE TG_ARGV[2] INTO m USING (r ->> TG_ARGV[1])::uuid;
                END IF;
            ELSE m := NULL; -- 'database'
        END CASE;
        models := models || m;
    END LOOP;
    PERFORM runtime.note_data_changes(models);
    RETURN NULL;
END $$;

-- note_statement_change: a statement trigger's models, from the rows the
-- statement changed (TG_ARGV[0]: 'model' or 'revision'). A statement that
-- changed no row notes nothing.
CREATE OR REPLACE FUNCTION runtime.note_statement_change() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog AS $$
DECLARE
    models uuid[];
BEGIN
    IF TG_ARGV[0] = 'model' THEN
        IF TG_OP = 'INSERT' THEN
            SELECT array_agg(DISTINCT model_id) INTO models FROM new_rows;
        ELSIF TG_OP = 'DELETE' THEN
            SELECT array_agg(DISTINCT model_id) INTO models FROM old_rows;
        ELSE
            SELECT array_agg(DISTINCT model_id) INTO models
            FROM (SELECT model_id FROM old_rows UNION SELECT model_id FROM new_rows) u;
        END IF;
    ELSE
        IF TG_OP = 'INSERT' THEN
            SELECT array_agg(DISTINCT rv.model_id) INTO models
            FROM (SELECT DISTINCT revision_id FROM new_rows) x LEFT JOIN model.revision rv ON rv.id = x.revision_id;
        ELSIF TG_OP = 'DELETE' THEN
            SELECT array_agg(DISTINCT rv.model_id) INTO models
            FROM (SELECT DISTINCT revision_id FROM old_rows) x LEFT JOIN model.revision rv ON rv.id = x.revision_id;
        ELSE
            SELECT array_agg(DISTINCT rv.model_id) INTO models
            FROM (SELECT revision_id FROM old_rows UNION SELECT revision_id FROM new_rows) x
            LEFT JOIN model.revision rv ON rv.id = x.revision_id;
        END IF;
    END IF;
    IF models IS NOT NULL THEN
        PERFORM runtime.note_data_changes(models);
    END IF;
    RETURN NULL;
END $$;

-- data_generation is a model's generation, read in one statement (one
-- snapshot), so a prune committing meanwhile leaves it as it was.
CREATE OR REPLACE FUNCTION runtime.data_generation(model uuid) RETURNS bigint
LANGUAGE sql STABLE SECURITY DEFINER SET search_path = pg_catalog AS $$
    SELECT coalesce((SELECT sum(g.pruned) FROM runtime.data_generation g
                     WHERE g.model_id IN (model, '00000000-0000-0000-0000-000000000000')), 0)::bigint
         + (SELECT count(*) FROM runtime.data_change c
            WHERE c.model_id IN (model, '00000000-0000-0000-0000-000000000000'))
$$;

-- prune_data_changes folds the committed rows into each model's pruned, in
-- one statement: every generation is the same before and after.
CREATE OR REPLACE FUNCTION runtime.prune_data_changes() RETURNS bigint
LANGUAGE sql SECURITY DEFINER SET search_path = pg_catalog AS $$
    WITH d AS (DELETE FROM runtime.data_change RETURNING model_id),
    c AS (SELECT model_id, count(*) AS n FROM d GROUP BY model_id),
    u AS (INSERT INTO runtime.data_generation AS g (model_id, pruned) SELECT model_id, n FROM c
          ON CONFLICT (model_id) DO UPDATE SET pruned = g.pruned + EXCLUDED.pruned RETURNING 1)
    SELECT count(*) FROM u
$$;

-- watch_data_changes makes changes to tbl count for the model each row
-- names (model_of, via_column, via_sql: see the file comment): per row,
-- with updated_at and the ignored columns not counting as a change, or per
-- statement (model_of 'model' or 'revision' only). Run again, it replaces
-- the table's triggers.
CREATE OR REPLACE FUNCTION runtime.watch_data_changes(
    tbl regclass, model_of text, via_column text DEFAULT NULL, via_sql text DEFAULT NULL,
    ignored text[] DEFAULT '{}', per_statement boolean DEFAULT false)
RETURNS void LANGUAGE plpgsql AS $$
DECLARE
    trg text;
    args text;
BEGIN
    IF model_of NOT IN ('model', 'self', 'revision', 'parent', 'database') THEN
        RAISE EXCEPTION 'watch_data_changes(%): unknown model_of %', tbl, model_of;
    END IF;
    IF model_of = 'parent' AND (via_column IS NULL OR via_sql IS NULL) THEN
        RAISE EXCEPTION 'watch_data_changes(%): a parent lookup needs via_column and via_sql', tbl;
    END IF;
    IF per_statement AND model_of NOT IN ('model', 'revision') THEN
        RAISE EXCEPTION 'watch_data_changes(%): per statement only by model or revision', tbl;
    END IF;
    ignored := ignored || ARRAY['updated_at'];
    FOR trg IN SELECT tgname FROM pg_trigger WHERE tgrelid = tbl AND tgname LIKE 'data_change%' AND NOT tgisinternal LOOP
        EXECUTE format('DROP TRIGGER %I ON %s', trg, tbl);
    END LOOP;
    IF per_statement THEN
        EXECUTE format('CREATE TRIGGER data_change AFTER INSERT ON %s REFERENCING NEW TABLE AS new_rows
            FOR EACH STATEMENT EXECUTE FUNCTION runtime.note_statement_change(%L)', tbl, model_of);
        EXECUTE format('CREATE TRIGGER data_change_update AFTER UPDATE ON %s REFERENCING OLD TABLE AS old_rows NEW TABLE AS new_rows
            FOR EACH STATEMENT EXECUTE FUNCTION runtime.note_statement_change(%L)', tbl, model_of);
        EXECUTE format('CREATE TRIGGER data_change_delete AFTER DELETE ON %s REFERENCING OLD TABLE AS old_rows
            FOR EACH STATEMENT EXECUTE FUNCTION runtime.note_statement_change(%L)', tbl, model_of);
    ELSE
        args := format('%L, %L, %L', model_of, coalesce(via_column, ''), coalesce(via_sql, ''));
        EXECUTE format('CREATE TRIGGER data_change AFTER INSERT OR DELETE ON %s
            FOR EACH ROW EXECUTE FUNCTION runtime.note_row_change(%s)', tbl, args);
        EXECUTE format('CREATE TRIGGER data_change_update AFTER UPDATE ON %s
            FOR EACH ROW WHEN ((to_jsonb(OLD) - %L::text[]) IS DISTINCT FROM (to_jsonb(NEW) - %L::text[]))
            EXECUTE FUNCTION runtime.note_row_change(%s)', tbl, ignored, ignored, args);
    END IF;
    EXECUTE format('CREATE TRIGGER data_change_truncate AFTER TRUNCATE ON %s
        FOR EACH STATEMENT EXECUTE FUNCTION runtime.note_row_change(%L)', tbl, 'database');
END $$;

-- unwatched_data_tables lists the tables of the watched schemas that have
-- no data_change trigger and are not left out on purpose (partitions follow
-- their parent).
CREATE OR REPLACE FUNCTION runtime.unwatched_data_tables() RETURNS SETOF text
LANGUAGE sql STABLE AS $$
    SELECT n.nspname || '.' || c.relname
    FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
      AND n.nspname IN ('core', 'identity', 'model', 'runtime', 'security', 'workflow')
      AND n.nspname || '.' || c.relname <> ALL (ARRAY[
          -- the generation itself
          'runtime.data_change', 'runtime.data_generation',
          -- written on a clock: the calculation's claim, storage figures
          'runtime.calc_engine_state', 'runtime.model_storage', 'runtime.storage_mark', 'runtime.table_compaction',
          -- integration bookkeeping and claims: what they load lands in fact_input
          'model.integration_attempt', 'model.integration_connection', 'model.integration_def',
          'model.integration_oauth_state', 'model.integration_run', 'model.integration_schedule',
          -- never read by a grid read
          'core.branding', 'core.integration_template', 'core.schema_version', 'core.starter_model',
          'core.tenant_credential', 'identity.api_key', 'identity.refused_invitation', 'identity.scim_token',
          'identity.sso_provider'])
      AND NOT EXISTS (SELECT 1 FROM pg_trigger t WHERE t.tgrelid = c.oid AND t.tgname = 'data_change')
    ORDER BY 1
$$;

DO $$
DECLARE
    t text;
    w record;
    has_model boolean;
    has_revision boolean;
BEGIN
    -- Tables reaching their model through a parent row, the large tables
    -- noted per statement, and the bookkeeping columns some tables update
    -- on a clock.
    CREATE TEMP TABLE data_change_plan (tbl text PRIMARY KEY, model_of text, via_column text, via_sql text,
                                        ignored text[] DEFAULT '{}', per_statement boolean DEFAULT false) ON COMMIT DROP;
    INSERT INTO data_change_plan (tbl, model_of, via_column, via_sql, ignored, per_statement) VALUES
        ('runtime.calc_result', 'model', NULL, NULL, '{}', true),
        ('runtime.fact_input', 'model', NULL, NULL, '{}', true),
        ('runtime.fact_input_history', 'model', NULL, NULL, '{}', true),
        ('runtime.metric_partition_state', 'model', NULL, NULL, '{}', true),
        ('runtime.form_record_posting', 'revision', NULL, NULL, '{}', true),
        ('core.model', 'self', NULL, NULL, '{}', false),
        ('core.customer', 'database', NULL, NULL, ARRAY['usage_checked_at', 'storage_bytes'], false),
        ('identity.user', 'database', NULL, NULL, ARRAY['last_seen_at'], false),
        ('model.dimension_member', 'parent', 'dimension_id', 'SELECT model_id FROM model.dimension_def WHERE id = $1', '{}', false),
        ('model.dimension_property', 'parent', 'dimension_id', 'SELECT model_id FROM model.dimension_def WHERE id = $1', '{}', false),
        ('model.dashboard_widget', 'parent', 'dashboard_id', 'SELECT model_id FROM model.dashboard_def WHERE id = $1', '{}', false),
        ('model.grid_dimension', 'parent', 'grid_id', 'SELECT model_id FROM model.grid_def WHERE id = $1', '{}', false),
        ('model.grid_metric', 'parent', 'grid_id', 'SELECT model_id FROM model.grid_def WHERE id = $1', '{}', false),
        ('model.metric_dimension_rule', 'parent', 'metric_id', 'SELECT model_id FROM model.metric_def WHERE id = $1', '{}', false),
        ('model.calc_dependency', 'parent', 'metric_id', 'SELECT model_id FROM model.metric_def WHERE id = $1', '{}', false),
        ('runtime.form_record', 'parent', 'form_id', 'SELECT model_id FROM model.form_def WHERE id = $1', '{}', false),
        ('runtime.record_code_counter', 'parent', 'form_id', 'SELECT model_id FROM model.form_def WHERE id = $1', '{}', false),
        ('runtime.record_field_history', 'parent', 'record_id',
            'SELECT f.model_id FROM runtime.form_record r JOIN model.form_def f ON f.id = r.form_id WHERE r.id = $1', '{}', false),
        ('workflow.automation_rule', 'revision', NULL, NULL, ARRAY['next_fire_at', 'last_fire_at'], false),
        ('workflow.workflow_instance', 'parent', 'workflow_def_id',
            'SELECT rv.model_id FROM workflow.workflow_def d JOIN model.revision rv ON rv.id = d.revision_id WHERE d.id = $1', '{}', false),
        ('workflow.workflow_step', 'parent', 'instance_id',
            'SELECT rv.model_id FROM workflow.workflow_instance i JOIN workflow.workflow_def d ON d.id = i.workflow_def_id
             JOIN model.revision rv ON rv.id = d.revision_id WHERE i.id = $1', ARRAY['reminded_at'], false),
        ('workflow.execution', 'parent', 'rule_id',
            'SELECT rv.model_id FROM workflow.automation_rule a JOIN model.revision rv ON rv.id = a.revision_id WHERE a.id = $1', '{}', false);

    FOR t IN SELECT * FROM runtime.unwatched_data_tables() LOOP
        SELECT * INTO w FROM data_change_plan WHERE tbl = t;
        IF FOUND THEN
            PERFORM runtime.watch_data_changes(t::regclass, w.model_of, w.via_column, w.via_sql, w.ignored, w.per_statement);
            CONTINUE;
        END IF;
        -- The rest name their model in a column, their revision, or nothing.
        SELECT bool_or(attname = 'model_id'), bool_or(attname = 'revision_id') INTO has_model, has_revision
        FROM pg_attribute WHERE attrelid = t::regclass AND attnum > 0 AND NOT attisdropped;
        PERFORM runtime.watch_data_changes(t::regclass,
            CASE WHEN has_model THEN 'model' WHEN has_revision THEN 'revision' ELSE 'database' END);
    END LOOP;
END $$;
