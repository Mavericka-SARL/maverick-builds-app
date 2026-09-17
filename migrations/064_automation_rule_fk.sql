-- automation_rule.source_form_id/source_grid_id (added in 044) were plain
-- UUID columns with no FK at all — unlike the sibling workflow_def_id
-- (same migration) and rollup_source_grid_id (055), which both correctly
-- use ON DELETE SET NULL. Deleting a form or grid silently and
-- permanently disabled any automation rule scoped to it: no error, no
-- audit trail, DispatchEventRules' exact-match WHERE clause just never
-- matched again.

-- Clear any pre-existing dangling references before adding the
-- constraint, in case a form/grid was already deleted under the old,
-- unenforced schema.
UPDATE workflow.automation_rule ar
SET source_form_id = NULL
WHERE ar.source_form_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM model.form_def f WHERE f.id = ar.source_form_id);

UPDATE workflow.automation_rule ar
SET source_grid_id = NULL
WHERE ar.source_grid_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM model.grid_def g WHERE g.id = ar.source_grid_id);

ALTER TABLE workflow.automation_rule
    ADD CONSTRAINT automation_rule_source_form_id_fkey
        FOREIGN KEY (source_form_id) REFERENCES model.form_def(id) ON DELETE SET NULL,
    ADD CONSTRAINT automation_rule_source_grid_id_fkey
        FOREIGN KEY (source_grid_id) REFERENCES model.grid_def(id) ON DELETE SET NULL;
