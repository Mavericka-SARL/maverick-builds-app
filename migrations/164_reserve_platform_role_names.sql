-- 164: a business role may not be named like a platform role (owner's
-- decision, 2026-10-06). A step or notification naming developer,
-- tenant_admin or another identity.user_role value means that platform role
-- (internal/workflow/assignee), so a business role of that name assigned its
-- steps to the platform's people and never to its own members, with no
-- message. Existing ones are renamed with a suffix; every path that creates
-- or renames a role (Business Admin, the AI Developer, SCIM groups) refuses
-- the names, and this constraint holds whatever path writes.
--
-- The list is identity.user_role's values: a value added to that enum is
-- added here too.
UPDATE identity.business_role
SET name = btrim(name) || ' (business role)'
WHERE lower(btrim(name)) IN ('platform_admin', 'tenant_admin', 'developer', 'business_admin', 'business_user');

ALTER TABLE identity.business_role
    ADD CONSTRAINT business_role_name_not_platform_role
    CHECK (lower(btrim(name)) NOT IN ('platform_admin', 'tenant_admin', 'developer', 'business_admin', 'business_user'));
