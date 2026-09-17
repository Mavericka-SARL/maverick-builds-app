-- Schema: model
-- Dimensions, metrics, hierarchies, scenarios, versions, dependency graph

CREATE SCHEMA IF NOT EXISTS model;

CREATE TABLE model.dimension_def (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id   UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    properties JSONB NOT NULL DEFAULT '[]',   -- [{name, type, required}]
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, name)
);

CREATE INDEX ON model.dimension_def (model_id);

CREATE TABLE model.dimension_member (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    dimension_id UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    code         TEXT NOT NULL,
    label        TEXT NOT NULL,
    parent_id    UUID REFERENCES model.dimension_member(id) ON DELETE SET NULL,
    properties   JSONB NOT NULL DEFAULT '{}',
    sort_order   INT NOT NULL DEFAULT 0,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (dimension_id, code)
);

CREATE INDEX ON model.dimension_member (dimension_id);
CREATE INDEX ON model.dimension_member (parent_id);

CREATE TABLE model.hierarchy (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    dimension_id UUID NOT NULL REFERENCES model.dimension_def(id) ON DELETE CASCADE,
    level_names  TEXT[] NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, name)
);

CREATE INDEX ON model.hierarchy (model_id);

CREATE TABLE model.metric_def (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    formula      TEXT,   -- NULL means input metric (no formula)
    storage_type core.storage_type NOT NULL DEFAULT 'oltp',
    is_input     BOOLEAN NOT NULL DEFAULT false,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, name)
);

CREATE INDEX ON model.metric_def (model_id);

-- Directed dependency graph edge: metric_id depends on depends_on_metric_id
CREATE TABLE model.calc_dependency (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    metric_id           UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    depends_on_metric_id UUID NOT NULL REFERENCES model.metric_def(id) ON DELETE CASCADE,
    dependency_type     TEXT NOT NULL DEFAULT 'direct',
    UNIQUE (metric_id, depends_on_metric_id)
);

CREATE INDEX ON model.calc_dependency (metric_id);
CREATE INDEX ON model.calc_dependency (depends_on_metric_id);

CREATE TABLE model.scenario (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, name)
);

CREATE INDEX ON model.scenario (model_id);

CREATE TABLE model.version (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    is_locked   BOOLEAN NOT NULL DEFAULT false,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (model_id, name)
);

CREATE INDEX ON model.version (model_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON model.dimension_def
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON model.metric_def
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
