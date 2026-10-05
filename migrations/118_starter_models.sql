-- The starter models (internal/starter: the tour and one guide per role) a
-- tenant holds, by the key that names each for good, and what each was last
-- brought to. internal/startersync reads it on every start to give tenants
-- that signed up earlier the starters they lack, and to add a starter's
-- current content as a new live revision of the model that holds it.
--
-- model_id goes NULL when the tenant deletes the model: the row stays, so a
-- starter someone removed is not put back. revision_id is the revision the
-- sync installed; when another revision of the model has been made live
-- since, the model is the tenant's own and is left alone.

CREATE TABLE IF NOT EXISTS core.starter_model (
    customer_id  UUID NOT NULL REFERENCES core.customer(id) ON DELETE CASCADE,
    starter_key  TEXT NOT NULL,
    model_id     UUID REFERENCES core.model(id) ON DELETE SET NULL,
    revision_id  UUID REFERENCES model.revision(id) ON DELETE SET NULL,
    content_hash TEXT NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (customer_id, starter_key)
);
