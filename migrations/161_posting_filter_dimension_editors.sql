-- 161: a posting's row filter, and who may change a dimension's members.
--
-- A register that keeps several measures in one table (the operations
-- register: produced tonnes, available capacity, debt draws, each row naming
-- its measure in a code column) posted every row into one metric: a model
-- needed an extra dimension for the code and a SUMIFS per line to take one
-- measure back out. row_filter is a list of conditions on the record's
-- fields, [{"field", "is": [values]} or {"field", "is_not": [values]}], all
-- of which a record meets to post; [] posts every record, as before.
--
-- A dimension business users keep (a GL account's reporting category, the
-- members of a table that keeps a dimension) could be changed by any of
-- them. editor_roles names the business roles that may add, change and
-- delete its members and properties; {} leaves it as before. The
-- application's administrators and the model's developers always may.
ALTER TABLE model.form_metric_mapping ADD COLUMN IF NOT EXISTS row_filter jsonb NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE model.dimension_def ADD COLUMN IF NOT EXISTS editor_roles text[] NOT NULL DEFAULT '{}';
