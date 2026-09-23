-- Per-tenant credentials (2026-09-21): what a tenant hands the platform to
-- reach its own external systems — today a Google service account for
-- private Google Sheets; an OAuth connection is the next kind. One row per
-- (tenant, kind); the secret half sealed by internal/integration's
-- credential encryption (INTEGRATION_CRED_KEY, mandatory, AAD-bound to the
-- tenant and the kind, so a row copied to another tenant cannot be opened
-- there); the public half — the account's e-mail, the project — in meta,
-- which is what the console shows.
CREATE TABLE IF NOT EXISTS core.tenant_credential (
    customer_id UUID NOT NULL REFERENCES core.customer(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,
    meta        JSONB NOT NULL DEFAULT '{}'::jsonb,
    secret_enc  TEXT NOT NULL,
    created_by  UUID,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, kind)
);
