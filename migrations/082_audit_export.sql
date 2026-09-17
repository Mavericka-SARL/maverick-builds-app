-- Audit export and retention (enterprise feature `audit_export`).
--
-- Export needs no schema: audit.audit_event is streamed as CSV or JSON
-- Lines. Retention is one setting per database: how many days of events to
-- keep, 0 meaning forever. A non-zero value has a floor of 30 days — an
-- audit log that can be emptied by a setting is not an audit log — and the
-- export is how events are archived before the sweep removes them.

CREATE TABLE IF NOT EXISTS audit.settings (
    id             BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    retention_days INT         NOT NULL DEFAULT 0 CHECK (retention_days = 0 OR retention_days >= 30),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO audit.settings (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING;

-- Retention sweeps and exports both walk by time; the id tiebreak keeps a
-- cursor stable across events with the same timestamp.
CREATE INDEX IF NOT EXISTS audit_event_occurred_id_idx ON audit.audit_event (occurred_at, id);
