-- Test-only: core.application.default_model_id (migrations/071_default_model.sql in the real
-- schema), read by appWorkingModelQuery when the trigger-event catalog
-- resolves an application's working model.
ALTER TABLE core.application ADD COLUMN default_model_id UUID;
