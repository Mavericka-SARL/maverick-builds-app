-- One calc_result row per cell.
--
-- A recalculation used to insert a metric's '{}' total on every pass without
-- removing the last one, and two passes at once could each insert a cell,
-- so a cell could hold many rows; readers take the newest (calc_at DESC,
-- id DESC). The write path now keeps one row per cell
-- (calculation.Store.ReplaceResultsTx); this removes the older rows already
-- there — on one development database, 97,000 of 197,000.

DELETE FROM runtime.calc_result c
USING (
    SELECT id, calc_at,
           row_number() OVER (PARTITION BY model_id, revision_id, metric_id, dim_members
                              ORDER BY calc_at DESC, id DESC) AS n
    FROM runtime.calc_result
) older
WHERE c.id = older.id AND c.calc_at = older.calc_at AND older.n > 1;
