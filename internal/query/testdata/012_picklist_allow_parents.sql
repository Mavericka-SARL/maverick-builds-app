-- A pick-list cell holds a LEAF member of its dimension unless the metric
-- allows parents (an approval level, a region that may be a group). An
-- employee's Department list offered "All Departments", and a row filed
-- under it matched no department in any SUMIFS, silently dropping out of the
-- plan (found 2026-10-05 rebuilding an HR planning workbook). Cells already
-- holding a parent keep it; new writes are checked.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS picklist_allow_parents BOOLEAN NOT NULL DEFAULT false;
