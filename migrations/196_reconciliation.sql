-- Workbook reconciliation (2026-11-21, internal/reconcile): a revision keeps
-- the workbook it rebuilds, the map of the workbook's cells in model terms
-- (rules), the cases (scenarios) to compare, and the last run's report. One
-- per revision; copied with the revision and carried by a model export.
CREATE TABLE IF NOT EXISTS model.reconciliation (
    revision_id UUID        PRIMARY KEY REFERENCES model.revision(id) ON DELETE CASCADE,
    file_name   TEXT        NOT NULL DEFAULT '',
    workbook    BYTEA,
    uploaded_by UUID        REFERENCES identity.user(id) ON DELETE SET NULL,
    uploaded_at TIMESTAMPTZ,
    map         JSONB       NOT NULL DEFAULT '{"rules": []}'::jsonb,
    cases       JSONB       NOT NULL DEFAULT '[]'::jsonb,
    status      TEXT        NOT NULL DEFAULT 'idle' CHECK (status IN ('idle', 'running', 'done', 'failed')),
    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    error       TEXT,
    report      JSONB
);
