-- REST API connector persistence (see internal/integration).
--
-- Four pieces: reusable application-scoped credential connections, connector
-- metadata on integration_def, a schedule table, and a durable run queue the
-- dedicated worker claims with FOR UPDATE SKIP LOCKED, plus a per-request
-- attempt log. Secrets live ONLY in integration_connection.secret_enc,
-- AES-256-GCM sealed by internal/integration/credentials.go with the
-- (application, connection) pair as associated data — a row copied to
-- another application fails authentication rather than decrypting.

-- ── Reusable credential connections (application-scoped) ─────────────────────
CREATE TABLE model.integration_connection (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    -- none | api_key | bearer | basic | oauth2_client_credentials
    -- (oauth2_authorization_code reserved; deferred by explicit decision)
    auth_type      TEXT NOT NULL,
    -- Public, non-secret metadata (token URL, scopes, username for basic…).
    meta           JSONB NOT NULL DEFAULT '{}',
    -- Versioned AEAD payload ("iv1:…"); never returned by any API.
    secret_enc     TEXT NOT NULL DEFAULT '',
    created_by     UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, name)
);
CREATE INDEX ON model.integration_connection (application_id);
GRANT SELECT, INSERT, UPDATE, DELETE ON model.integration_connection TO role_model;

-- ── Connector metadata on integration_def ────────────────────────────────────
ALTER TABLE model.integration_def
    ADD COLUMN IF NOT EXISTS direction        TEXT NOT NULL DEFAULT 'pull',
    ADD COLUMN IF NOT EXISTS enabled          BOOL NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS connection_id    UUID REFERENCES model.integration_connection(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS config_version   INT  NOT NULL DEFAULT 1,
    -- Activation gate: hash of the config that last passed a test request;
    -- editing request-critical config changes the hash and invalidates it.
    ADD COLUMN IF NOT EXISTS last_tested_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS last_tested_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS description      TEXT NOT NULL DEFAULT '';

-- ── Schedules ────────────────────────────────────────────────────────────────
CREATE TABLE model.integration_schedule (
    integration_id UUID PRIMARY KEY REFERENCES model.integration_def(id) ON DELETE CASCADE,
    -- manual | interval | cron  (daily/weekly compile to cron client-side)
    kind             TEXT NOT NULL DEFAULT 'manual',
    interval_seconds INT,
    cron_expr        TEXT NOT NULL DEFAULT '',
    timezone         TEXT NOT NULL DEFAULT 'UTC',
    enabled          BOOL NOT NULL DEFAULT false,
    overlap_policy   TEXT NOT NULL DEFAULT 'skip',  -- skip | queue
    misfire_policy   TEXT NOT NULL DEFAULT 'skip',  -- skip | fire_now
    -- The developer whose enable action this schedule runs under (audit
    -- principal; execution visibility is developer-role by decision).
    enabled_by   UUID,
    next_fire_at TIMESTAMPTZ,
    last_fire_at TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
GRANT SELECT, INSERT, UPDATE, DELETE ON model.integration_schedule TO role_model;

-- ── Run queue (extends the existing history table into a durable queue) ──────
ALTER TABLE model.integration_run
    -- queued | running | success | partial | failed | cancelled
    ADD COLUMN IF NOT EXISTS trigger_type  TEXT NOT NULL DEFAULT 'manual', -- manual | schedule | test | dry_run
    ADD COLUMN IF NOT EXISTS dry_run       BOOL NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS scheduled_for TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS claimed_by    TEXT NOT NULL DEFAULT '',      -- worker instance id
    ADD COLUMN IF NOT EXISTS lease_until   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS attempt_count INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS started_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS finished_at   TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS http_status   INT  NOT NULL DEFAULT 0,       -- last/representative status
    ADD COLUMN IF NOT EXISTS duration_ms   INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS pages         INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS requests      INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS retries       INT  NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS records_read    INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS records_written INT NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS records_skipped INT NOT NULL DEFAULT 0,
    -- Machine-readable failure class: dns | blocked_host | tls | timeout |
    -- auth | rate_limit | http_error | invalid_data | too_large | cancelled |
    -- internal. Human text stays in `message` (always sanitized).
    ADD COLUMN IF NOT EXISTS error_code TEXT NOT NULL DEFAULT '',
    -- Sanitized structured extras (never headers, never bodies, never URLs
    -- with credentials): {"host":"api.example.com","records_path":"$.data"}
    ADD COLUMN IF NOT EXISTS meta JSONB NOT NULL DEFAULT '{}';

-- Claim scans: the worker looks for queued runs whose lease is free.
CREATE INDEX IF NOT EXISTS integration_run_queue
    ON model.integration_run (status, created_at)
    WHERE status IN ('queued', 'running');
-- One scheduled run per (integration, tick): the scheduler INSERTs with
-- ON CONFLICT DO NOTHING, so double-firing scheduler replicas collapse.
CREATE UNIQUE INDEX IF NOT EXISTS integration_run_one_per_tick
    ON model.integration_run (integration_id, scheduled_for)
    WHERE scheduled_for IS NOT NULL;
GRANT UPDATE ON model.integration_run TO role_model;

-- ── Per-request attempts ─────────────────────────────────────────────────────
CREATE TABLE model.integration_attempt (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    run_id      UUID NOT NULL REFERENCES model.integration_run(id) ON DELETE CASCADE,
    seq         INT  NOT NULL,                 -- 1-based within the run
    page        INT  NOT NULL DEFAULT 0,
    method      TEXT NOT NULL,
    -- scheme://host/path only — query stripped (may carry api-key params).
    url_sanitized TEXT NOT NULL,
    http_status INT  NOT NULL DEFAULT 0,
    duration_ms INT  NOT NULL DEFAULT 0,
    error_code  TEXT NOT NULL DEFAULT '',
    message     TEXT NOT NULL DEFAULT '',      -- sanitized
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (run_id, seq)
);
CREATE INDEX ON model.integration_attempt (run_id, seq);
GRANT SELECT, INSERT, DELETE ON model.integration_attempt TO role_model;
