-- Migration 144 (tables keeping a dimension, who edits a table, field
-- history) as far as these tests need it.
ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS keeps_dimension_id UUID,
    ADD COLUMN IF NOT EXISTS member_label_field TEXT,
    ADD COLUMN IF NOT EXISTS member_root_label TEXT,
    ADD COLUMN IF NOT EXISTS edit_access TEXT NOT NULL DEFAULT 'creator';
CREATE TABLE IF NOT EXISTS runtime.record_field_history (
    id         BIGSERIAL PRIMARY KEY,
    record_id  UUID NOT NULL REFERENCES runtime.form_record(id) ON DELETE CASCADE,
    field      TEXT NOT NULL,
    old_value  JSONB,
    new_value  JSONB,
    changed_by UUID,
    changed_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);
