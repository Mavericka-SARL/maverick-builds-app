-- 166: hiding a member of a property grouping hides the members it groups
-- (owner's decision, 2026-11-08). A dimension that groups another
-- dimension's members by a property (dimension_def.source_dimension_id and
-- source_property: regions <- employees.region) has no parent link to them,
-- so hiding EMEA there hid none of the employees whose region is EMEA, and
-- their facts stayed visible to the person meant not to see EMEA (private
-- observations, 2026-09-28).
--
-- Every check reads identity.effective_access_rule, so the cascade is made
-- there: each hidden member rule on a grouping member adds a hidden rule on
-- every member of the source dimension whose property (its key matched
-- regardless of case, as internal/rollup.PropertyValue does) is that
-- member's code. The rules an administrator sets and sees are the tables'
-- rows; only the view carries the derived ones. The base is NOT
-- MATERIALIZED so a check's filter on user_id still reaches the tables'
-- indexes.
CREATE OR REPLACE VIEW identity.effective_access_rule AS
WITH base AS NOT MATERIALIZED (
    SELECT r.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, NULL::uuid AS role_id
    FROM identity.user_access_rule r
    UNION ALL
    SELECT m.user_id, r.rule_type, r.ref_id, r.access, r.ref_lineage_id, r.role_id
    FROM identity.business_role_access_rule r
    JOIN identity.business_role_member m ON m.role_id = r.role_id
)
SELECT b.user_id, b.rule_type, b.ref_id, b.access, b.ref_lineage_id, b.role_id
FROM base b
UNION ALL
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
    WHERE lower(p.key) = lower(gd.source_property) AND p.value = gm.code);
