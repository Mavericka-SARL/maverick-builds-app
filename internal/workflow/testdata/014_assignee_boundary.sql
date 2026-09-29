-- Mirror of the parts of the real schema that the workspace boundary of
-- workflow assignment (internal/workflow/assignee) reads and this reduced
-- test schema lacked: an account's own tenant (identity.user.customer_id,
-- migrations/002_identity.sql), whether it is disabled
-- (identity.user.disabled_at, migrations/080_sso_scim.sql), explicit
-- application and model grants
-- (migrations/048_user_resource_access.sql) and the core.model those point
-- at. Store.IsAssigneeEligible and notification steps' role recipients use
-- the boundary. The boundary's own tests run on the real migrations
-- (internal/workflow/assignee/assigneetest); these only keep the tests on
-- this schema running.
--
-- With core.model present, PublishWorkflowDef no longer skips its re-home
-- to the active revision (its to_regclass guard), so the columns and the
-- model.dimension_def that statement reads come along. No test here creates
-- a model, so the re-home finds no active revision and changes nothing.

ALTER TABLE identity.user
    ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id),
    ADD COLUMN IF NOT EXISTS disabled_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS core.model (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id     UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    name               TEXT NOT NULL DEFAULT '',
    active_revision_id UUID
);

CREATE SCHEMA IF NOT EXISTS model;

CREATE TABLE IF NOT EXISTS model.dimension_def (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id    UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    revision_id UUID,
    name        TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS identity.user_app_access (
    user_id        UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, application_id)
);

CREATE TABLE IF NOT EXISTS identity.user_model_access (
    user_id  UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    model_id UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    PRIMARY KEY (user_id, model_id)
);
