-- Phase 3B: Form Records as internal integration source
-- Explicit mapping from form fields to input metrics with posting rules.

CREATE TABLE model.form_metric_mapping (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id           UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    form_id            UUID NOT NULL REFERENCES model.form_def(id) ON DELETE CASCADE,
    name               TEXT NOT NULL,
    source_field       TEXT NOT NULL,
    target_metric_id   UUID NOT NULL REFERENCES model.metric_def(id),
    aggregation        TEXT NOT NULL DEFAULT 'sum',
    posting_statuses   TEXT[] NOT NULL DEFAULT '{approved}',
    dimension_mappings JSONB NOT NULL DEFAULT '{}',  -- {dim_id: form_field_name}
    scenario_source    TEXT NOT NULL DEFAULT 'workflow_context',
    scenario_field     TEXT,
    scenario_value     TEXT,
    version_source     TEXT NOT NULL DEFAULT 'fixed',
    version_field      TEXT,
    version_value      TEXT NOT NULL DEFAULT 'draft',
    live_posting       BOOL NOT NULL DEFAULT TRUE,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON model.form_metric_mapping (model_id);
CREATE INDEX ON model.form_metric_mapping (form_id);

-- Posting log: one row per (mapping, form_record), replaced on re-post.
CREATE TABLE runtime.form_record_posting (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    mapping_id       UUID NOT NULL REFERENCES model.form_metric_mapping(id) ON DELETE CASCADE,
    form_record_id   UUID NOT NULL REFERENCES runtime.form_record(id) ON DELETE CASCADE,
    target_metric_id UUID NOT NULL,
    scenario         TEXT NOT NULL,
    version          TEXT NOT NULL,
    dim_members      JSONB NOT NULL DEFAULT '{}',
    posted_value     NUMERIC,
    posted_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(mapping_id, form_record_id)
);

CREATE INDEX ON runtime.form_record_posting (mapping_id);
CREATE INDEX ON runtime.form_record_posting (form_record_id);

-- Tag fact_input rows that originate from form postings so aggregation can replace them.
ALTER TABLE runtime.fact_input ADD COLUMN IF NOT EXISTS source_ref UUID;
CREATE INDEX IF NOT EXISTS fact_input_source_ref_idx ON runtime.fact_input (source_ref) WHERE source_ref IS NOT NULL;
