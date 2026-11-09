-- 170: a dimension's property names are unique regardless of case (owner's
-- decision, 2026-11-09). Formulas read a property whatever its case, but
-- 017's UNIQUE(dimension_id, name) compares the exact spelling, so the CSV
-- member import and the REST connector — which lower-case a header and
-- declared it as written — left a developer's "Segment" next to an
-- imported "segment", the imported values under the second key, where no
-- formula reading the developer's spelling saw them; two concurrent
-- declarations could do the same (observations, 2026-09-28).
--
-- Each dimension's declarations that differ only in case merge into the
-- oldest one — the name the developer chose, which formulas and groupings
-- use — keeping its type. Every member's values under the other spellings
-- move under it; where a member holds several, the newest declaration's
-- value wins, since a variant was only ever made by an import after the
-- original existed. A grouping by another spelling reads the kept one. A
-- notice names each dimension merged. A unique index on lower(name) then
-- keeps it so.
DO $$
DECLARE
    g record;
BEGIN
    FOR g IN
        SELECT p.dimension_id,
               d.name AS dimension_name,
               lower(p.name) AS lname,
               (array_agg(p.id ORDER BY p.created_at, p.id))[1] AS keep_id,
               (array_agg(p.name ORDER BY p.created_at, p.id))[1] AS keep_name,
               array_agg(p.name ORDER BY p.created_at, p.id) AS names
        FROM model.dimension_property p
        JOIN model.dimension_def d ON d.id = p.dimension_id
        GROUP BY p.dimension_id, d.name, lower(p.name)
        HAVING count(*) > 1
    LOOP
        -- jsonb_object_agg keeps the last value of a repeated key: the kept
        -- spelling's own value first, an undeclared spelling next, then the
        -- declared spellings oldest to newest.
        UPDATE model.dimension_member m
        SET properties = (
            SELECT jsonb_object_agg(
                       CASE WHEN lower(e.key) = g.lname THEN g.keep_name ELSE e.key END, e.value
                       ORDER BY CASE WHEN lower(e.key) <> g.lname THEN 0
                                     WHEN e.key = g.keep_name THEN 1
                                     ELSE 2 + COALESCE(array_position(g.names, e.key), 0) END)
            FROM jsonb_each(m.properties) AS e
        )
        WHERE m.dimension_id = g.dimension_id
          AND EXISTS (SELECT 1 FROM jsonb_object_keys(m.properties) AS k
                      WHERE lower(k) = g.lname AND k <> g.keep_name);

        UPDATE model.dimension_def
        SET source_property = g.keep_name
        WHERE source_dimension_id = g.dimension_id
          AND lower(source_property) = g.lname AND source_property <> g.keep_name;

        DELETE FROM model.dimension_property
        WHERE dimension_id = g.dimension_id AND lower(name) = g.lname AND id <> g.keep_id;

        RAISE NOTICE 'migration 170: dimension % (%): property declarations % merged into %',
            g.dimension_name, g.dimension_id, array_to_string(g.names, ', '), g.keep_name;
    END LOOP;
END $$;

CREATE UNIQUE INDEX IF NOT EXISTS dimension_property_name_any_case_key
    ON model.dimension_property (dimension_id, lower(name));
