-- 168: "only these members" (owner's decision, 2026-11-08). Access rules
-- hid or restricted named members; there was no allow-list, so a manager
-- restricted to a few accounts saw every account added after their rules
-- were set until an administrator hid it (observations, 2026-10-17). A
-- member rule may now be 'only' (the member stays writable) or 'only_read'
-- (read-only): it puts the member on the person's allow-list for its
-- dimension, and every other member of that dimension is hidden from them —
-- members added later included — except the allowed members' own ancestors
-- (the totals above them) and descendants.
--
-- As for property groupings (166), every check reads
-- identity.effective_access_rule, so the allow-list is applied there: an
-- 'only' rule reads as 'write' and 'only_read' as 'read', and each other
-- member of the dimension gets a derived hidden rule. The rules an
-- administrator sets and sees stay the tables' rows.
ALTER TABLE identity.user_access_rule DROP CONSTRAINT IF EXISTS user_access_rule_access_check;
ALTER TABLE identity.user_access_rule
    ADD CONSTRAINT user_access_rule_access_check
    CHECK (access IN ('write', 'read', 'hidden', 'only', 'only_read')
           AND (access NOT IN ('only', 'only_read') OR rule_type = 'dimension_member'));
ALTER TABLE identity.business_role_access_rule DROP CONSTRAINT IF EXISTS business_role_access_rule_access_check;
ALTER TABLE identity.business_role_access_rule
    ADD CONSTRAINT business_role_access_rule_access_check
    CHECK (access IN ('write', 'read', 'hidden', 'only', 'only_read')
           AND (access NOT IN ('only', 'only_read') OR rule_type = 'dimension_member'));

CREATE OR REPLACE VIEW identity.effective_access_rule AS
WITH RECURSIVE raw AS NOT MATERIALIZED (
    SELECT r.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, NULL::uuid AS role_id
    FROM identity.user_access_rule r
    UNION ALL
    SELECT m.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, r.role_id
    FROM identity.business_role_access_rule r
    JOIN identity.business_role_member m ON m.role_id = r.role_id
),
base AS NOT MATERIALIZED (
    SELECT user_id, rule_type, ref_id,
           CASE access WHEN 'only' THEN 'write' WHEN 'only_read' THEN 'read' ELSE access END AS access,
           ref_lineage_id, role_id
    FROM raw
),
-- Each person's allowed members, in every revision (by lineage).
allowed AS (
    SELECT DISTINCT r.user_id, m.id AS member_id, m.dimension_id
    FROM raw r
    JOIN model.dimension_member m
      ON r.rule_type = 'dimension_member' AND r.access IN ('only', 'only_read')
     AND (m.lineage_id = r.ref_lineage_id OR (r.ref_lineage_id IS NULL AND m.id::text = lower(r.ref_id)))
),
below AS (
    SELECT user_id, member_id FROM allowed
    UNION
    SELECT b.user_id, c.id FROM below b JOIN model.dimension_member c ON c.parent_member_id = b.member_id
),
above AS (
    SELECT user_id, member_id FROM allowed
    UNION
    SELECT a.user_id, p.parent_member_id FROM above a
    JOIN model.dimension_member p ON p.id = a.member_id AND p.parent_member_id IS NOT NULL
)
SELECT b.user_id, b.rule_type, b.ref_id, b.access, b.ref_lineage_id, b.role_id
FROM base b
UNION ALL
-- 166: a hidden grouping member hides the source members it groups.
SELECT DISTINCT b.user_id, 'dimension_member'::text, sm.id::text, 'hidden'::text, sm.lineage_id, b.role_id
FROM base b
JOIN model.dimension_member gm
  ON b.rule_type = 'dimension_member' AND b.access = 'hidden'
 AND (gm.lineage_id = b.ref_lineage_id
      OR (b.ref_lineage_id IS NULL AND gm.id::text = lower(b.ref_id)))
JOIN model.dimension_def gd
  ON gd.id = gm.dimension_id AND gd.source_dimension_id IS NOT NULL AND COALESCE(gd.source_property, '') <> ''
JOIN model.dimension_member sm ON sm.dimension_id = gd.source_dimension_id
WHERE EXISTS (
    SELECT 1 FROM jsonb_each_text(COALESCE(sm.properties, '{}'::jsonb)) p
    WHERE lower(p.key) = lower(gd.source_property) AND p.value = gm.code)
UNION ALL
-- 168: every member of an allow-listed dimension that is not allowed, nor
-- above or below an allowed one, is hidden.
SELECT DISTINCT d.user_id, 'dimension_member'::text, m.id::text, 'hidden'::text, m.lineage_id, NULL::uuid
FROM (SELECT DISTINCT user_id, dimension_id FROM allowed) d
JOIN model.dimension_member m ON m.dimension_id = d.dimension_id
WHERE NOT EXISTS (SELECT 1 FROM below k WHERE k.user_id = d.user_id AND k.member_id = m.id)
  AND NOT EXISTS (SELECT 1 FROM above k WHERE k.user_id = d.user_id AND k.member_id = m.id);
