-- 173: the values an input metric accepts (owner's decision, 2026-11-09: a
-- small generic rule set on input metrics, refusing writes).
--
-- A budget template wants obviously wrong inputs refused, not only tinted:
-- units and costs not negative, a price within a range, a headcount whole.
-- Highlight rules (tint a cell) and closed rules (lock it) were the nearest
-- things; a negative number saved anywhere.
--
-- model.metric_def.input_rules: {} for none, or any of
--   "min": n      no value below n
--   "max": n      no value above n
--   "whole": true whole numbers only
-- in the metric's stored units (a percentage metric stores 15 for 15%).
-- They apply to a numeric input metric's values: number, percentage and
-- currency — not to a text, date, boolean or pick-list metric's cells. A
-- clear is always accepted.
ALTER TABLE model.metric_def ADD COLUMN IF NOT EXISTS input_rules jsonb NOT NULL DEFAULT '{}'::jsonb;

-- The guard below first asks whether any metric has rules at all.
CREATE INDEX IF NOT EXISTS metric_def_with_input_rules ON model.metric_def (id) WHERE input_rules <> '{}'::jsonb;

-- Whether rules accept v. The one statement of the rules: the guard, the
-- console's cell writes and the imports' row checks all ask it.
CREATE OR REPLACE FUNCTION model.input_value_accepted(rules jsonb, v numeric) RETURNS boolean
LANGUAGE sql IMMUTABLE AS $$
	SELECT v IS NULL OR rules IS NULL OR (
		    (NOT rules ? 'min' OR v >= (rules->>'min')::numeric)
		AND (NOT rules ? 'max' OR v <= (rules->>'max')::numeric)
		AND (NOT COALESCE((rules->>'whole')::boolean, false) OR v = trunc(v)))
$$;

-- A number as the person reads it: a percentage with its sign.
CREATE OR REPLACE FUNCTION model.input_number_text(v numeric, fmt text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
	SELECT trim_scale(v)::text || CASE WHEN fmt = 'percentage' THEN '%' ELSE '' END
$$;

-- What rules accept, in words: "values from 0 to 100, whole numbers only".
CREATE OR REPLACE FUNCTION model.input_rules_text(rules jsonb, fmt text) RETURNS text
LANGUAGE sql IMMUTABLE AS $$
	SELECT concat_ws(', ',
		CASE
			WHEN rules ? 'min' AND rules ? 'max' THEN format('values from %s to %s',
				model.input_number_text((rules->>'min')::numeric, fmt), model.input_number_text((rules->>'max')::numeric, fmt))
			WHEN rules ? 'min' THEN format('values of at least %s', model.input_number_text((rules->>'min')::numeric, fmt))
			WHEN rules ? 'max' THEN format('values of at most %s', model.input_number_text((rules->>'max')::numeric, fmt))
		END,
		CASE WHEN COALESCE((rules->>'whole')::boolean, false) THEN 'whole numbers only' END)
$$;

-- The sentence refusing v at the cell dims ({dimension id: member code}) of
-- the metric m — "Units accepts values of at least 0: -5 refused at JAN /
-- NORTH" — or NULL when m's rules accept it or do not apply to its cells.
CREATE OR REPLACE FUNCTION model.input_rule_refusal(m model.metric_def, v numeric, dims jsonb) RETURNS text
LANGUAGE sql STABLE AS $$
	SELECT format('%s accepts %s: %s refused', COALESCE(NULLIF(m.label, ''), m.name),
			model.input_rules_text(m.input_rules, m.format), model.input_number_text(v, m.format))
		|| COALESCE(' at ' || (SELECT string_agg(e.value, ' / ' ORDER BY e.value) FROM jsonb_each_text(dims) e), '')
	WHERE m.input_rules <> '{}'::jsonb
	  AND m.picklist_dimension_id IS NULL
	  AND COALESCE(NULLIF(m.format, ''), 'number') IN ('number', 'percentage', 'currency')
	  AND NOT model.input_value_accepted(m.input_rules, v)
$$;

-- runtime.input_rules_guard refuses a statement storing a value its metric's
-- rules refuse, on whichever path it came: a typed or pasted cell, a file
-- import, a connector, the AI Developer, the chat connector. Statement-level,
-- with the statement's rows as a transition table: an import of 100,000 rows
-- is one check, and none when no metric has rules.
--
-- Values not entered but carried are not checked: a revision copy, a model
-- import, a workflow's copy on approval, a restructure moving values (a
-- metric attached to a grid, a member split) and a text note converted say
-- so for their transaction (mvx.skip_input_rules): a rule added after its
-- values were stored must not stop the next revision being made. Nor are the
-- totals a table's records post (source_ref, the mapping): they are posted
-- in the background, where a refusal would leave the total silently stale,
-- and a record's own fields are checked where it is saved.
CREATE OR REPLACE FUNCTION runtime.input_rules_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
	refusal text;
BEGIN
	IF COALESCE(current_setting('mvx.skip_input_rules', true), '') = 'on'
	   OR NOT EXISTS (SELECT 1 FROM model.metric_def WHERE input_rules <> '{}'::jsonb) THEN
		RETURN NULL;
	END IF;
	SELECT r.refusal INTO refusal
	FROM new_rows n
	JOIN model.metric_def m ON m.id = n.metric_id AND m.input_rules <> '{}'::jsonb
	CROSS JOIN LATERAL (SELECT model.input_rule_refusal(m, n.value, n.dim_members) AS refusal) r
	WHERE n.text_value IS NULL AND n.source_ref IS NULL AND r.refusal IS NOT NULL
	LIMIT 1;
	IF refusal IS NOT NULL THEN
		RAISE EXCEPTION USING ERRCODE = 'MV422', MESSAGE = refusal;
	END IF;
	RETURN NULL;
END $$;

DROP TRIGGER IF EXISTS input_rules_guard ON runtime.fact_input;
CREATE TRIGGER input_rules_guard
	AFTER INSERT ON runtime.fact_input
	REFERENCING NEW TABLE AS new_rows
	FOR EACH STATEMENT EXECUTE FUNCTION runtime.input_rules_guard();
