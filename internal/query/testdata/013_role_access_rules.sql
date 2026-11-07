-- Test-schema mirror of migrations/156_role_access_rules.sql: the view every
-- access check reads, a person's own rules and their business roles'. The
-- role tables are the minimum the view joins.
CREATE TABLE IF NOT EXISTS identity.business_role (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID,
    name         TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS identity.business_role_member (
    role_id UUID NOT NULL REFERENCES identity.business_role(id) ON DELETE CASCADE,
    user_id UUID NOT NULL,
    PRIMARY KEY (role_id, user_id)
);
CREATE TABLE IF NOT EXISTS identity.business_role_access_rule (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id        UUID NOT NULL REFERENCES identity.business_role(id) ON DELETE CASCADE,
    rule_type      TEXT NOT NULL,
    ref_id         TEXT NOT NULL,
    access         TEXT NOT NULL DEFAULT 'write',
    ref_lineage_id UUID,
    UNIQUE (role_id, rule_type, ref_id)
);
CREATE OR REPLACE VIEW identity.effective_access_rule AS
SELECT r.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, NULL::uuid AS role_id
FROM identity.user_access_rule r
UNION ALL
SELECT m.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, r.role_id
FROM identity.business_role_access_rule r
JOIN identity.business_role_member m ON m.role_id = r.role_id;
