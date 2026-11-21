-- Two table settings a developer chooses (2026-11-21, help desk):
-- record_scope: '' (as before) — members' access rules narrow who sees the
-- table's records, and a record posts within its creator's access;
-- 'open' — everyone who opens the table sees every record, and a save posts
-- within the access of the person saving it (a manager hands a ticket an
-- agent logged to another team).
-- existing_codes: what a file row naming a value for the table's auto code
-- does: 'refuse' the row, or 'update' the record holding that code. '' keeps
-- tables made before this as they were (the value is ignored and the row
-- adds a record); new tables are created with 'refuse'.
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS record_scope text NOT NULL DEFAULT ''
    CHECK (record_scope IN ('', 'open'));
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS existing_codes text NOT NULL DEFAULT ''
    CHECK (existing_codes IN ('', 'refuse', 'update'));
