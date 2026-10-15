-- Tables of records (model.form_def, the developer console's Records tab).
--
-- 1. A record's status is optional. A table has a status only when its
--    developer gives it a Status field; a record of a table without one has
--    no status (NULL) and counts in its metrics as soon as it is saved.
ALTER TABLE runtime.form_record ALTER COLUMN status DROP NOT NULL;
ALTER TABLE runtime.form_record ALTER COLUMN status DROP DEFAULT;

-- 2. Every table that exists already keeps working as it did: it gets a
--    Status field (named "status", or "record_status" when a field of its
--    own is called "status"), so its records keep their statuses and new
--    ones still start as drafts. Its developer may remove the field.
UPDATE model.form_def fd
SET fields = f.arr || jsonb_build_array(jsonb_build_object(
        'name', CASE WHEN EXISTS (SELECT 1 FROM jsonb_array_elements(f.arr) e WHERE e->>'name' = 'status')
                     THEN 'record_status' ELSE 'status' END,
        'label', 'Status', 'type', 'status', 'required', false))
FROM (SELECT id, CASE WHEN jsonb_typeof(fields) = 'array' THEN fields ELSE '[]'::jsonb END AS arr
      FROM model.form_def) f
WHERE f.id = fd.id
  AND NOT EXISTS (SELECT 1 FROM jsonb_array_elements(f.arr) e WHERE e->>'type' = 'status');

-- 3. A table may show each person only the records they created (its
--    application's administrators still see every record).
ALTER TABLE model.form_def ADD COLUMN IF NOT EXISTS own_records_only BOOLEAN NOT NULL DEFAULT false;

-- 4. The last number an auto-code field gave out, per table and field, so a
--    deleted record's code is never given to another. A table copied into a
--    new revision or another model starts from its records' highest code.
CREATE TABLE IF NOT EXISTS runtime.record_code_counter (
    form_id    UUID   NOT NULL REFERENCES model.form_def(id) ON DELETE CASCADE,
    field      TEXT   NOT NULL,
    last_value BIGINT NOT NULL,
    PRIMARY KEY (form_id, field)
);

-- 5. A records widget offers "+ New record" only when its developer adds the
--    button. The widgets that exist keep the button they showed.
UPDATE model.dashboard_widget
SET widget_props = (CASE WHEN jsonb_typeof(widget_props) = 'object' THEN widget_props ELSE '{}'::jsonb END)
                   || '{"new_record_button": true}'::jsonb
WHERE widget_type = 'form';

-- 6. A table-to-metric mapping names only its own model's table, metric and
--    grid (crudapp.CheckMappingRefs). The developer API stored whatever ids a
--    body named, so a mapping could name another tenant's table and post its
--    records into this model's facts. One naming another model's table or
--    metric goes, with the totals it posted; another model's grid is cleared.
SELECT set_config('mvx.delete_reason', 'foreign_mapping_removed', true);
DELETE FROM runtime.fact_input WHERE source_ref IN (
    SELECT m.id FROM model.form_metric_mapping m
    JOIN model.form_def fd ON fd.id = m.form_id
    JOIN model.metric_def md ON md.id = m.target_metric_id
    WHERE fd.model_id <> m.model_id OR md.model_id <> m.model_id);
DELETE FROM model.form_metric_mapping m
USING model.form_def fd, model.metric_def md
WHERE fd.id = m.form_id AND md.id = m.target_metric_id
  AND (fd.model_id <> m.model_id OR md.model_id <> m.model_id);
SELECT set_config('mvx.delete_reason', '', true);
UPDATE model.form_metric_mapping m SET grid_id = NULL
WHERE m.grid_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM model.grid_def g WHERE g.id = m.grid_id AND g.model_id = m.model_id);
