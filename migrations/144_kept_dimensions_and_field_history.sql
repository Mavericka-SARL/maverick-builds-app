-- Tables that keep a dimension's members, tables everyone may edit, and the
-- history of a record's fields.
--
-- 1. A table may keep the members of a dimension of its revision: each of
--    its records is one member, its auto-code field the member's code and
--    one of its fields the member's label. Adding a record adds the member,
--    renaming the record renames it and deleting the record deletes it, so
--    the same list is a table people work in and a dimension that access
--    rules, grids, dashboards and other tables' dimension fields name. A
--    dimension is kept by at most one table.
--    member_root_label, when set, names a total member (code ALL) the
--    records' members sit under — the "All accounts" a dashboard selector
--    opens on.
ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS keeps_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS member_label_field TEXT,
    ADD COLUMN IF NOT EXISTS member_root_label TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS form_def_keeps_dimension
    ON model.form_def (keeps_dimension_id) WHERE keeps_dimension_id IS NOT NULL;

-- 2. Who may change a table's records: their creator and the application's
--    administrators (every table so far), or everyone who sees them.
ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS edit_access TEXT NOT NULL DEFAULT 'creator'
        CHECK (edit_access IN ('creator', 'everyone'));

-- 3. The members of a kept dimension change only with the table's records.
--    Every path that writes members — the developer console, the AI
--    developer, a business-maintained dimension, an import, a connector —
--    is refused with this message; the records path says it is the one
--    writing (mvx.kept_member_write, for its transaction). A member removed
--    by a cascade (its dimension, revision or model deleted) is not
--    refused: pg_trigger_depth() > 1 inside a foreign key's action.
CREATE OR REPLACE FUNCTION model.guard_kept_member() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    dim   uuid;
    owner text;
BEGIN
    IF pg_trigger_depth() > 1 OR COALESCE(current_setting('mvx.kept_member_write', true), '') = 'on' THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.code IS NOT DISTINCT FROM OLD.code AND NEW.label IS NOT DISTINCT FROM OLD.label
       AND NEW.dimension_id = OLD.dimension_id THEN
        RETURN NEW;
    END IF;
    dim := CASE WHEN TG_OP = 'DELETE' THEN OLD.dimension_id ELSE NEW.dimension_id END;
    SELECT COALESCE(NULLIF(btrim(label), ''), name) INTO owner
    FROM model.form_def WHERE keeps_dimension_id = dim LIMIT 1;
    IF owner IS NULL AND TG_OP = 'UPDATE' AND NEW.dimension_id <> OLD.dimension_id THEN
        SELECT COALESCE(NULLIF(btrim(label), ''), name) INTO owner
        FROM model.form_def WHERE keeps_dimension_id = OLD.dimension_id LIMIT 1;
    END IF;
    IF owner IS NULL THEN
        RETURN CASE WHEN TG_OP = 'DELETE' THEN OLD ELSE NEW END;
    END IF;
    RAISE EXCEPTION USING
        ERRCODE = 'MV409',
        MESSAGE = format('the members of this dimension are the records of the table "%s": add, rename or delete its records instead', owner);
END $$;

DROP TRIGGER IF EXISTS kept_member_guard ON model.dimension_member;
CREATE TRIGGER kept_member_guard
    BEFORE INSERT OR DELETE OR UPDATE OF code, label, dimension_id ON model.dimension_member
    FOR EACH ROW EXECUTE FUNCTION model.guard_kept_member();

-- 4. The history of the fields a table keeps one for (a field's "history"
--    option): each value a record's field took, who gave it and when. It
--    goes with its record.
CREATE TABLE IF NOT EXISTS runtime.record_field_history (
    id         BIGSERIAL PRIMARY KEY,
    record_id  UUID NOT NULL REFERENCES runtime.form_record(id) ON DELETE CASCADE,
    field      TEXT NOT NULL,
    old_value  JSONB,
    new_value  JSONB,
    changed_by UUID REFERENCES identity.user(id) ON DELETE SET NULL,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
CREATE INDEX IF NOT EXISTS record_field_history_record ON runtime.record_field_history (record_id, changed_at, id);
CREATE INDEX IF NOT EXISTS record_field_history_changed_by ON runtime.record_field_history (changed_by);
