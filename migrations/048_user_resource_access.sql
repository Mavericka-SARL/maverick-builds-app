-- Per-user whitelists for app-level and model-level access.
-- If a user has NO rows in user_app_access, they see all apps in their workspaces.
-- If they have ANY rows, they see only the listed apps (and implicitly all models within).
-- Same whitelist logic applies to user_model_access within a visible app.
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
