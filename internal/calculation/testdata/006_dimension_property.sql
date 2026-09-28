-- Mirror migrations/017_dimension_properties.sql: the declared property
-- schema (name + data_type) the scheduler reads to type dim.property and
-- criteria ranges. Member values stay in dimension_member.properties.
CREATE TABLE IF NOT EXISTS model.dimension_property (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id  UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    data_type     TEXT NOT NULL DEFAULT 'text',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(dimension_id, name)
);
