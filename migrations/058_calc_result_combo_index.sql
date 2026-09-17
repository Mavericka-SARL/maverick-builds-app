-- grid() is about to start bulk-reading every calc metric's every combo in
-- one query (DISTINCT ON (metric_id, dim_members) ... ORDER BY metric_id,
-- dim_members, calc_at DESC), not just the '{}' aggregate row it read
-- before. The existing index only covers (model_id, revision_id, metric_id)
-- — it narrows to one metric's rows but can't satisfy the DISTINCT ON's own
-- ordering, so Postgres would sort every one of that metric's historical
-- rows in memory on every request. This index is a strict superset (same
-- leading columns, plus dim_members/calc_at) that satisfies both the new
-- bulk read and the pre-existing '{}'-only read with no extra sort step.
CREATE INDEX IF NOT EXISTS calc_result_combo_idx
    ON runtime.calc_result (model_id, revision_id, metric_id, dim_members, calc_at DESC);

-- Superseded by the index above for every query shape in the codebase.
-- calc_result now takes a per-leaf-combo write on every recalculation
-- (WriteCalcResults), so avoiding a second index to maintain on every insert
-- is worth it.
DROP INDEX IF EXISTS runtime.calc_result_revision_idx;
