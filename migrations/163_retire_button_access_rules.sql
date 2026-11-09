-- 163: access rules of type 'button' are retired (2026-11-08). The rule
-- tables accepted them since 020, but no screen created one and nothing
-- read one: a button rule restricted nothing. They also named no model or
-- tenant, so removing someone from a tenant left them behind, to apply
-- again if the person came back (observations, 2026-09-30). The rule
-- writers now refuse the type; the rows go, and the tables take only the
-- two types that are enforced.
DELETE FROM identity.user_access_rule WHERE rule_type = 'button';
DELETE FROM identity.business_role_access_rule WHERE rule_type = 'button';

ALTER TABLE identity.user_access_rule DROP CONSTRAINT IF EXISTS user_access_rule_rule_type_check;
ALTER TABLE identity.user_access_rule
    ADD CONSTRAINT user_access_rule_rule_type_check CHECK (rule_type IN ('dimension_member', 'metric'));

ALTER TABLE identity.business_role_access_rule DROP CONSTRAINT IF EXISTS business_role_access_rule_rule_type_check;
ALTER TABLE identity.business_role_access_rule
    ADD CONSTRAINT business_role_access_rule_rule_type_check CHECK (rule_type IN ('dimension_member', 'metric'));
