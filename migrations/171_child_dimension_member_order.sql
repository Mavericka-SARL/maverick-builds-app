-- 171: a child dimension's members list in Build's order everywhere (owner's
-- decision, 2026-11-09: grids follow the Build order).
--
-- A dimension whose members hang off another dimension's members
-- (dimension_def.parent_dimension_id) is shown in Developer › Dimensions
-- grouped by that parent, in the parent dimension's order, members without
-- one last, each group in its own order. Every other reader — grids,
-- selectors, charts, exports, the API — lists members flat by sort_order,
-- where members added across two parents in turn stayed interleaved
-- (A1 of P, B1 of Q, A2 of P) and a member added later went to the end.
--
-- sort_order now follows the grouping: after every statement that adds
-- members to such a dimension, re-parents or reorders them, or reorders its
-- parent dimension, the child dimension is renumbered — groups in the parent
-- dimension's order, unassigned members last, each group keeping its
-- members' order (one added without an order at its group's end). One rule on the table
-- serves every path that writes members: the console, imports, connectors,
-- the AI Developer, a model import and a revision copy.

-- appended: the members a statement just inserted. One inserted without an
-- order (sort_order 0, the column's default) goes to its group's end; one
-- given an order keeps it.
CREATE OR REPLACE FUNCTION model.regroup_child_dimension(dim uuid, appended uuid[] DEFAULT '{}') RETURNS void
LANGUAGE sql AS $$
	WITH RECURSIVE
	members AS (
		SELECT m.id, m.code, m.parent_member_id, m.sort_order,
		       (m.sort_order = 0 AND m.id = ANY(appended)) AS unordered
		FROM model.dimension_member m
		WHERE m.dimension_id = dim
	),
	-- The members at the top of the dimension's own tree (no parent, or a
	-- parent outside it), grouped by their parent in the parent dimension.
	tops AS (
		SELECT m.id,
		       row_number() OVER (
		           ORDER BY (pm.id IS NULL), pm.sort_order, pm.code, m.parent_member_id::text, m.unordered, m.sort_order, m.code
		       ) AS k
		FROM members m
		JOIN model.dimension_def d ON d.id = dim
		LEFT JOIN members own ON own.id = m.parent_member_id
		LEFT JOIN model.dimension_member pm ON pm.id = m.parent_member_id AND pm.dimension_id = d.parent_dimension_id
		WHERE d.parent_dimension_id IS NOT NULL AND own.id IS NULL
	),
	-- Each member's place among its siblings in the dimension's own tree.
	siblings AS (
		SELECT m.id, row_number() OVER (PARTITION BY m.parent_member_id ORDER BY m.unordered, m.sort_order, m.code) AS k
		FROM members m
	),
	-- The dimension's own hierarchy under each top, in its order: a parent
	-- before its children (a cycle, which the edit paths refuse, is cut).
	tree(id, path) AS (
		SELECT id, ARRAY[k] FROM tops
		UNION ALL
		SELECT c.id, t.path || s.k
		FROM members c
		JOIN tree t ON c.parent_member_id = t.id
		JOIN siblings s ON s.id = c.id
		WHERE cardinality(t.path) < 64
	),
	ranked AS (
		SELECT id, row_number() OVER (ORDER BY path) AS rn FROM tree
	)
	UPDATE model.dimension_member m SET sort_order = r.rn
	FROM ranked r
	WHERE r.id = m.id AND m.sort_order IS DISTINCT FROM r.rn;
$$;

-- The dimensions a statement's rows touch: a child dimension whose members
-- they are, and the child dimensions of a dimension whose members they are.
-- The renumbering's own UPDATE fires this again; it then changes nothing, and
-- a statement changing no row regroups nothing.
CREATE OR REPLACE FUNCTION model.dimension_member_regroup() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
	dim uuid;
BEGIN
	IF pg_trigger_depth() > 4 THEN
		RETURN NULL;
	END IF;
	FOR dim IN
		SELECT DISTINCT d.id FROM model.dimension_def d
		WHERE d.parent_dimension_id IS NOT NULL
		  AND (d.id IN (SELECT dimension_id FROM changed_members)
		       OR d.parent_dimension_id IN (SELECT dimension_id FROM changed_members))
	LOOP
		PERFORM model.regroup_child_dimension(dim, ARRAY(SELECT id FROM changed_members WHERE dimension_id = dim));
	END LOOP;
	RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS dimension_member_regroup_insert ON model.dimension_member;
CREATE TRIGGER dimension_member_regroup_insert
	AFTER INSERT ON model.dimension_member
	REFERENCING NEW TABLE AS changed_members
	FOR EACH STATEMENT EXECUTE FUNCTION model.dimension_member_regroup();

-- An update regroups only when it moved a member to another parent or
-- changed an order: a label or a property edit leaves the order alone.
-- (A trigger naming columns, UPDATE OF, cannot have transition tables.)
CREATE OR REPLACE FUNCTION model.dimension_member_regroup_on_update() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
	dim uuid;
BEGIN
	IF pg_trigger_depth() > 4 THEN
		RETURN NULL;
	END IF;
	FOR dim IN
		WITH moved AS (
			SELECT n.dimension_id FROM changed_members n JOIN old_members o ON o.id = n.id
			WHERE n.parent_member_id IS DISTINCT FROM o.parent_member_id OR n.sort_order IS DISTINCT FROM o.sort_order
		)
		SELECT DISTINCT d.id FROM model.dimension_def d
		WHERE d.parent_dimension_id IS NOT NULL
		  AND (d.id IN (SELECT dimension_id FROM moved) OR d.parent_dimension_id IN (SELECT dimension_id FROM moved))
	LOOP
		PERFORM model.regroup_child_dimension(dim);
	END LOOP;
	RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS dimension_member_regroup_update ON model.dimension_member;
CREATE TRIGGER dimension_member_regroup_update
	AFTER UPDATE ON model.dimension_member
	REFERENCING OLD TABLE AS old_members NEW TABLE AS changed_members
	FOR EACH STATEMENT EXECUTE FUNCTION model.dimension_member_regroup_on_update();

-- A dimension that becomes a child dimension, or changes parent.
CREATE OR REPLACE FUNCTION model.dimension_def_regroup() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
	IF NEW.parent_dimension_id IS NOT NULL AND NEW.parent_dimension_id IS DISTINCT FROM OLD.parent_dimension_id THEN
		PERFORM model.regroup_child_dimension(NEW.id);
	END IF;
	RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS dimension_def_regroup ON model.dimension_def;
CREATE TRIGGER dimension_def_regroup
	AFTER UPDATE OF parent_dimension_id ON model.dimension_def
	FOR EACH ROW EXECUTE FUNCTION model.dimension_def_regroup();

-- Every existing child dimension, once.
SELECT model.regroup_child_dimension(id) FROM model.dimension_def WHERE parent_dimension_id IS NOT NULL;
