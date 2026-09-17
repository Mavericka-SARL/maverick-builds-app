-- Dimension properties: typed attributes on dimension members usable in formulas.

CREATE TABLE IF NOT EXISTS model.dimension_property (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id  UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    data_type     TEXT NOT NULL DEFAULT 'text',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(dimension_id, name)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON model.dimension_property TO role_model;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA model TO role_model;
