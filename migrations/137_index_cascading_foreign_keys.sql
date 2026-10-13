-- Deleting a large revision no longer reads the whole workspace once per
-- metric.
--
-- A revision's metrics and members go by foreign-key cascade, and
-- PostgreSQL runs each cascade once per deleted row: one
-- "DELETE FROM runtime.calc_result WHERE metric_id = $1" for every metric,
-- one "UPDATE model.dimension_member SET parent_member_id = NULL WHERE
-- parent_member_id = $1" for every member. None of these columns led an
-- index, so each of those statements read its whole table. On 2026-10-13 a
-- revision of 618 metrics, in a workspace holding 400,000 calculated
-- cells, took minutes to delete; the person reloaded, the request was
-- cancelled and the delete rolled back, and the console showed nothing.
--
-- An index led by each column turns those reads into lookups. A metric
-- column holds few distinct values, which a B-tree stores once with the
-- list of rows, so these indexes stay small.

CREATE INDEX IF NOT EXISTS calc_result_metric_idx            ON runtime.calc_result            (metric_id);
CREATE INDEX IF NOT EXISTS fact_input_metric_idx             ON runtime.fact_input             (metric_id);
CREATE INDEX IF NOT EXISTS metric_partition_state_metric_idx ON runtime.metric_partition_state (metric_id);
CREATE INDEX IF NOT EXISTS form_metric_mapping_target_idx    ON model.form_metric_mapping      (target_metric_id);
CREATE INDEX IF NOT EXISTS dimension_member_parent_member_idx ON model.dimension_member        (parent_member_id);
