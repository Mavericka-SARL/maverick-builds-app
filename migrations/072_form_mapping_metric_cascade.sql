-- Revision delete returned HTTP 500 via form_metric_mapping_target_metric_id_fkey:
-- deleting a revision cascades its metric_def rows, but a form_metric_mapping
-- row referencing one of those metrics blocked the cascade whenever the
-- mapping itself wasn't reached by the same statement's revision cascade —
-- e.g. a legacy mapping with revision_id NULL (predating 056's backfill)
-- whose target metric belongs to the deleted revision. A mapping without its
-- target metric is meaningless, so the FK now cascades: deleting the metric
-- (directly or via revision delete) removes the mapping, and the posting log
-- under it follows via form_record_posting's existing mapping_id cascade.
ALTER TABLE model.form_metric_mapping
    DROP CONSTRAINT form_metric_mapping_target_metric_id_fkey;
ALTER TABLE model.form_metric_mapping
    ADD CONSTRAINT form_metric_mapping_target_metric_id_fkey
    FOREIGN KEY (target_metric_id) REFERENCES model.metric_def(id) ON DELETE CASCADE;
