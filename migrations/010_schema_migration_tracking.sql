-- Schema: deployment.schema_migration
-- Tracks generated schema migrations per model (pending, applied, rolled_back, failed)

CREATE TABLE deployment.schema_migration (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    revision_id    UUID REFERENCES core.revision(id) ON DELETE SET NULL,
    version_number INT NOT NULL,
    status         TEXT NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending', 'applied', 'rolled_back', 'failed')),
    files          JSONB NOT NULL DEFAULT '[]',   -- [{filename, sql, rollback_sql, checksum}]
    error          TEXT,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    applied_at     TIMESTAMPTZ,
    rolled_back_at TIMESTAMPTZ,
    UNIQUE (model_id, version_number)
);

CREATE INDEX ON deployment.schema_migration (model_id, status);
