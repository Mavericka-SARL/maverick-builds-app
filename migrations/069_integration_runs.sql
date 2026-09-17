-- Per-integration run history: every execution of a saved integration
-- (CSV re-run, Google Sheets sync, dimension/form/grid target alike)
-- records its outcome, so an integration's own history is visible instead
-- of only the global import-job list.
CREATE TABLE IF NOT EXISTS model.integration_run (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    integration_id UUID NOT NULL REFERENCES model.integration_def(id) ON DELETE CASCADE,
    status         TEXT NOT NULL DEFAULT 'success',
    rows_imported  INT  NOT NULL DEFAULT 0,
    error_rows     INT  NOT NULL DEFAULT 0,
    message        TEXT NOT NULL DEFAULT '',
    run_by         UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS integration_run_by_integration
    ON model.integration_run (integration_id, created_at DESC);
GRANT SELECT, INSERT, DELETE ON model.integration_run TO role_model;
