-- CRUD Application Mode: form definitions and record storage

CREATE TABLE model.form_def (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    label      TEXT NOT NULL,
    fields     JSONB NOT NULL DEFAULT '[]',  -- [{name, label, type, required, options:[]}]
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(model_id, name)
);

CREATE TYPE runtime.record_status AS ENUM ('draft', 'submitted', 'approved', 'rejected');

CREATE TABLE runtime.form_record (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    form_id    UUID NOT NULL REFERENCES model.form_def(id) ON DELETE CASCADE,
    data       JSONB NOT NULL DEFAULT '{}',
    status     runtime.record_status NOT NULL DEFAULT 'draft',
    created_by UUID NOT NULL REFERENCES identity.user(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON runtime.form_record (form_id, status);
CREATE INDEX ON runtime.form_record (created_by);
