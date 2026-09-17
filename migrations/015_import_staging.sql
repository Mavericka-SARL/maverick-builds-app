-- Staging rows produced by the validate step; flushed to runtime.fact_input on commit.
CREATE TABLE import.import_staging (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    job_id      UUID NOT NULL REFERENCES import.import_job(id) ON DELETE CASCADE,
    metric_id   UUID NOT NULL,
    dim_members JSONB NOT NULL DEFAULT '{}',
    value       NUMERIC NOT NULL,
    row_number  INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON import.import_staging (job_id);
