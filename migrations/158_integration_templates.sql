-- An integration saved as a template, to make again in any model of its
-- tenant (2026-11-07: a saved loader or export could not be reused in
-- another model). Its target is kept by name and found by name where it is
-- used; its config names metrics, dimensions and fields, as a loader's
-- does. A connector that holds a connection or reads another model is not
-- a template: those belong to their model.
CREATE TABLE IF NOT EXISTS core.integration_template (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id UUID NOT NULL,
    name        TEXT NOT NULL,
    type        TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT 'grid',
    target_name TEXT NOT NULL DEFAULT '',
    config      JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_by  UUID,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (customer_id, name)
);
