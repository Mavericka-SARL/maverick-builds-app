-- A table's text field posts into a text metric (2026-11-21): an approved
-- demand override's reason beside its units in the forecast. Each record's
-- posting keeps the text it posts; the mapping's cell takes the latest
-- record's text (value 0 beside it, as every text cell, migration 111).
-- NULL for every number: a posting saved before carries none.
ALTER TABLE runtime.form_record_posting ADD COLUMN IF NOT EXISTS posted_text TEXT;
