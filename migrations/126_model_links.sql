-- Model links: a connector whose source is a grid of another model of the
-- same tenant (internal/integration Config.Protocol "model", run by the
-- gateway: internal/gateway/model_links.go).
--
-- link_id is a connector's identity across the revisions of its model: a
-- revision copy keeps it, a new connector (or a duplicate) gets a fresh one.
-- The source side's switch is written to every row that carries the link,
-- so promoting a revision copied before a switch-off cannot bring the link
-- back on.
--
-- source_model_id repeats config.model.model_id for the source side: its
-- developers and the tenant's administrators list the links that read a
-- model by it. It is set NULL when that model is deleted, and the link's
-- runs then fail.
--
-- source_switched_by has no foreign key, as integration_schedule.enabled_by:
-- a platform administrator switching a dedicated tenant's link is not a row
-- of that tenant's identity.user.
ALTER TABLE model.integration_def
    ADD COLUMN IF NOT EXISTS link_id            UUID NOT NULL DEFAULT gen_random_uuid(),
    ADD COLUMN IF NOT EXISTS source_model_id    UUID REFERENCES core.model(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS source_enabled     BOOL NOT NULL DEFAULT true,
    ADD COLUMN IF NOT EXISTS source_switched_by UUID,
    ADD COLUMN IF NOT EXISTS source_switched_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS integration_def_source_model
    ON model.integration_def (source_model_id) WHERE source_model_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS integration_def_link
    ON model.integration_def (link_id);
