-- Migration 139 (tables of records) as far as these tests need it, and the
-- member and person names a totals query labels its groups with.
ALTER TABLE runtime.form_record ALTER COLUMN status DROP NOT NULL;
ALTER TABLE runtime.form_record ALTER COLUMN status DROP DEFAULT;
ALTER TABLE runtime.form_record ALTER COLUMN created_by DROP NOT NULL;
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS own_records_only BOOLEAN NOT NULL DEFAULT false;
CREATE TABLE IF NOT EXISTS runtime.record_code_counter (
    form_id    UUID   NOT NULL REFERENCES model.form_def(id) ON DELETE CASCADE,
    field      TEXT   NOT NULL,
    last_value BIGINT NOT NULL,
    PRIMARY KEY (form_id, field)
);
ALTER TABLE identity.user ADD COLUMN IF NOT EXISTS display_name TEXT;
CREATE TABLE IF NOT EXISTS model.dimension_member (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id UUID NOT NULL,
    code         TEXT NOT NULL,
    label        TEXT NOT NULL DEFAULT ''
);
