-- Mirrors migrations/183_member_label_with_code.sql for this package's test schema.
-- A table keeping a dimension may label each member with its record's code
-- before its label field ("IC03 · Intragroup loan principal"): two records
-- with the same text — two intragroup loans — read alike in every grid, and
-- the code field cannot be the label (2026-11-13, consolidation kit).
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS member_label_with_code boolean NOT NULL DEFAULT false;
