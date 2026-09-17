CREATE TABLE IF NOT EXISTS model.dashboard_folder (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    parent_id  UUID REFERENCES model.dashboard_folder(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

ALTER TABLE model.dashboard_def
    ADD COLUMN IF NOT EXISTS folder_id UUID REFERENCES model.dashboard_folder(id) ON DELETE SET NULL;
