-- Unique constraint required by UpsertAttributeRule ON CONFLICT (application_id, name)
ALTER TABLE security.abac_rule
    ADD CONSTRAINT abac_rule_application_name_uq UNIQUE (application_id, name);
