-- A table may name who changes each of its records — the person a person
-- field names (form_def.editor_field), for the business roles it lists
-- (editor_roles): to a holder of one of those roles, the members of the
-- dimension the table keeps whose record names someone else are read-only.
-- They still read every member (the portfolio stays whole); records naming
-- such a member are read-only to them as any read-only member makes them.
-- Empty editor_field: nothing changes (owner decision 2026-11-27).
ALTER TABLE model.form_def
    ADD COLUMN IF NOT EXISTS editor_field text NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS editor_roles text[] NOT NULL DEFAULT '{}';

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
editors AS NOT MATERIALIZED (
    -- 200: a table naming who changes each of its records (form_def.editor_field,
    -- editor_roles): to a person holding one of those roles, every member of the
    -- table's dimension whose record does not name them in that person field is
    -- read-only — a project manager writes the projects whose record names them
    -- and reads the others, a new project included, without a rule per person
    -- (development-programme build: 1,400 rules for 14 project managers). Only
    -- members with a record: the table's total member (no record) stays as it
    -- is, so it never makes the person's own members read-only from above.
    SELECT DISTINCT rm.user_id, 'dimension_member'::text AS rule_type, m.id::text AS ref_id, 'read'::text AS access,
           m.lineage_id AS ref_lineage_id, rm.role_id
    FROM model.form_def f
    CROSS JOIN LATERAL (
        SELECT COALESCE(
            (SELECT e->>'name' FROM jsonb_array_elements(f.fields) e WHERE e->>'type' = 'autocode' LIMIT 1),
            (SELECT e->>'name' FROM jsonb_array_elements(f.fields) e WHERE e->>'code' = 'true' LIMIT 1)) AS code_field
    ) cf
    JOIN core.model mo ON mo.id = f.model_id
    JOIN core.application ap ON ap.id = mo.application_id
    -- A business role counts in the applications of its workspace and in the
    -- tenant-level applications (workspace_id NULL) of its tenant, as a step's
    -- roles do (internal/workflow/assignee).
    JOIN identity.business_role br ON br.name = ANY (f.editor_roles)
     AND (br.workspace_id = ap.workspace_id
          OR (ap.workspace_id IS NULL
              AND br.workspace_id IN (SELECT w.id FROM core.workspace w WHERE w.customer_id = ap.customer_id)))
    JOIN identity.business_role_member rm ON rm.role_id = br.id
    JOIN model.dimension_member m ON m.dimension_id = f.keeps_dimension_id
    WHERE f.editor_field <> '' AND f.keeps_dimension_id IS NOT NULL AND cf.code_field IS NOT NULL
      AND EXISTS (
          SELECT 1 FROM runtime.form_record r
          WHERE r.form_id = f.id AND r.data->>cf.code_field = m.code)
      AND NOT EXISTS (
          SELECT 1 FROM runtime.form_record r
          WHERE r.form_id = f.id AND r.data->>cf.code_field = m.code
            AND r.data->>f.editor_field = rm.user_id::text)
),
-- 200: the rules every later part reads: the stored ones and the person-field ones, so a
-- read-only project makes its activities read-only through the groupings (166, 188) as a
-- stored rule on it did.
ruled AS NOT MATERIALIZED (
    SELECT user_id, rule_type, ref_id, access, ref_lineage_id, role_id FROM base
    UNION ALL
    SELECT user_id, rule_type, ref_id, access, ref_lineage_id, role_id FROM editors
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
FROM ruled b
UNION ALL
-- 166, 188: a hidden or read-only grouping member makes the source members
-- it groups hidden or read-only.
SELECT DISTINCT b.user_id, 'dimension_member'::text, sm.id::text, b.access, sm.lineage_id, b.role_id
FROM ruled b
JOIN model.dimension_member gm
  ON b.rule_type = 'dimension_member' AND b.access IN ('hidden', 'read')
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
