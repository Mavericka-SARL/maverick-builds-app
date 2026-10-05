-- A pick-list metric: each of its cells holds a member of one dimension —
-- an activity's Region, a Status of Draft / Committed / Cancelled, a
-- Yes / No — as a dimension field of a form does. The cell stores the
-- member's key (formula.PicklistKey of its code), so facts stay numbers and
-- a revision copy, a model export and an import carry them unchanged.
-- Formulas read the member's code. NULL for every other format.
-- Deleting a dimension a pick-list uses is refused by the console and the
-- AI Developer; SET NULL only keeps revision and model deletes working.
ALTER TABLE model.metric_def
    ADD COLUMN IF NOT EXISTS picklist_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL;
