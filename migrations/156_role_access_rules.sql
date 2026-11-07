-- Access rules a business role carries: every member of the role holds them
-- as if they were their own (owner request, 2026-11-07: about 60 rules for
-- each of 11 people, and a new joiner got none). They are not copied into
-- identity.user_access_rule: identity.effective_access_rule is a person's own
-- rules and their roles' together, and every check reads that, so joining
-- or leaving a role, or changing its rules, takes effect at once. Where two
-- rules land on one member or metric, the stricter wins, as it always has.
CREATE TABLE IF NOT EXISTS identity.business_role_access_rule (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    role_id        UUID NOT NULL REFERENCES identity.business_role(id) ON DELETE CASCADE,
    rule_type      TEXT NOT NULL CHECK (rule_type IN ('dimension_member', 'metric', 'button')),
    ref_id         TEXT NOT NULL,
    access         TEXT NOT NULL DEFAULT 'write' CHECK (access IN ('write', 'read', 'hidden')),
    ref_lineage_id UUID,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (role_id, rule_type, ref_id)
);
CREATE INDEX IF NOT EXISTS business_role_access_rule_lineage_idx ON identity.business_role_access_rule (ref_lineage_id);

CREATE OR REPLACE VIEW identity.effective_access_rule AS
SELECT r.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, NULL::uuid AS role_id
FROM identity.user_access_rule r
UNION ALL
SELECT m.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, r.role_id
FROM identity.business_role_access_rule r
JOIN identity.business_role_member m ON m.role_id = r.role_id;
