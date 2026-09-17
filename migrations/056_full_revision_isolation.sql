-- Full revision isolation: forms, form-metric mappings, integrations,
-- dashboard folders, workflow definitions and automation rules become
-- revision-scoped, so a revision is a fully self-contained copy of the
-- model (migration 027 covered metrics/dimensions, 033/034 dashboards/
-- grids, 031/037 runtime facts).
--
-- Pattern per table (mirrors 027 + 028):
--   1. add revision_id FK -> model.revision
--   2. backfill existing rows to the model's active revision, falling back
--      to the earliest revision
--   3. replace model/app-wide unique names with per-revision uniqueness
--
-- Rows whose model has no revisions at all keep revision_id NULL; readers
-- treat NULL as "visible in every revision" for backward compatibility.

-- ─── model.form_def ──────────────────────────────────────────────────────────

ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.revision(id) ON DELETE CASCADE;

UPDATE model.form_def fd
SET revision_id = COALESCE(
    (SELECT m.active_revision_id FROM core.model m WHERE m.id = fd.model_id),
    (SELECT r.id FROM model.revision r WHERE r.model_id = fd.model_id ORDER BY r.created_at ASC LIMIT 1)
)
WHERE fd.revision_id IS NULL;

ALTER TABLE model.form_def DROP CONSTRAINT IF EXISTS form_def_model_id_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS form_def_rev_name_uq
    ON model.form_def (model_id, revision_id, name) NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS form_def_revision_idx ON model.form_def (revision_id);

-- ─── model.form_metric_mapping (column exists since 037; backfill only) ──────

UPDATE model.form_metric_mapping fmm
SET revision_id = COALESCE(
    (SELECT fd.revision_id FROM model.form_def fd WHERE fd.id = fmm.form_id),
    (SELECT m.active_revision_id FROM core.model m WHERE m.id = fmm.model_id)
)
WHERE fmm.revision_id IS NULL;

CREATE INDEX IF NOT EXISTS form_metric_mapping_revision_idx ON model.form_metric_mapping (revision_id);

-- ─── model.integration_def ───────────────────────────────────────────────────

ALTER TABLE model.integration_def
    ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.revision(id) ON DELETE CASCADE;

UPDATE model.integration_def i
SET revision_id = COALESCE(
    (SELECT m.active_revision_id FROM core.model m WHERE m.id = i.model_id),
    (SELECT r.id FROM model.revision r WHERE r.model_id = i.model_id ORDER BY r.created_at ASC LIMIT 1)
)
WHERE i.revision_id IS NULL;

CREATE INDEX IF NOT EXISTS integration_def_revision_idx ON model.integration_def (revision_id);

-- ─── model.dashboard_folder ──────────────────────────────────────────────────

ALTER TABLE model.dashboard_folder
    ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.revision(id) ON DELETE CASCADE;

UPDATE model.dashboard_folder f
SET revision_id = COALESCE(
    (SELECT m.active_revision_id FROM core.model m WHERE m.id = f.model_id),
    (SELECT r.id FROM model.revision r WHERE r.model_id = f.model_id ORDER BY r.created_at ASC LIMIT 1)
)
WHERE f.revision_id IS NULL;

CREATE INDEX IF NOT EXISTS dashboard_folder_revision_idx ON model.dashboard_folder (revision_id);

-- ─── workflow.workflow_def ───────────────────────────────────────────────────
-- Application-scoped; resolve the revision through the application's
-- earliest model (applications and models are effectively 1:1 today).

ALTER TABLE workflow.workflow_def
    ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.revision(id) ON DELETE CASCADE;

UPDATE workflow.workflow_def wd
SET revision_id = (
    SELECT COALESCE(
        m.active_revision_id,
        (SELECT r.id FROM model.revision r WHERE r.model_id = m.id ORDER BY r.created_at ASC LIMIT 1)
    )
    FROM core.model m
    WHERE m.application_id = wd.application_id
    ORDER BY m.created_at ASC
    LIMIT 1
)
WHERE wd.revision_id IS NULL;

ALTER TABLE workflow.workflow_def DROP CONSTRAINT IF EXISTS workflow_def_application_id_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS workflow_def_rev_name_uq
    ON workflow.workflow_def (application_id, revision_id, name) NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS workflow_def_revision_idx ON workflow.workflow_def (revision_id);

-- ─── workflow.automation_rule ────────────────────────────────────────────────

ALTER TABLE workflow.automation_rule
    ADD COLUMN IF NOT EXISTS revision_id UUID REFERENCES model.revision(id) ON DELETE CASCADE;

UPDATE workflow.automation_rule ar
SET revision_id = (
    SELECT COALESCE(
        m.active_revision_id,
        (SELECT r.id FROM model.revision r WHERE r.model_id = m.id ORDER BY r.created_at ASC LIMIT 1)
    )
    FROM core.model m
    WHERE m.application_id = ar.application_id
    ORDER BY m.created_at ASC
    LIMIT 1
)
WHERE ar.revision_id IS NULL;

-- CreateAutomationRule upserts ON CONFLICT (application_id, name); the
-- conflict target moves to the per-revision index (NULLS NOT DISTINCT so
-- legacy NULL-revision rules still dedupe by name).
ALTER TABLE workflow.automation_rule DROP CONSTRAINT IF EXISTS automation_rule_application_id_name_key;
CREATE UNIQUE INDEX IF NOT EXISTS automation_rule_rev_name_uq
    ON workflow.automation_rule (application_id, revision_id, name) NULLS NOT DISTINCT;
CREATE INDEX IF NOT EXISTS automation_rule_revision_idx ON workflow.automation_rule (revision_id);
