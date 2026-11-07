-- Test-only: model.grid_def, from which the trigger-event catalog lists a
-- "<grid>.changed" event per grid (the real table has many more columns).
CREATE TABLE model.grid_def (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    revision_id UUID
);
