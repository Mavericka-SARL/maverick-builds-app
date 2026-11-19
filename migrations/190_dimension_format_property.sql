-- A dimension whose members carry their own number format (Integrated FP&A
-- build, 2026-11-19): a Budget-vs-actual line dimension mixes amounts,
-- margins and headcount, so one metric over it showed "€35.0" for a gross
-- margin of 35.0% and a variance % of a percentage. format_property names one
-- of the dimension's properties; a member's value of it — percentage, number
-- or currency — is the format of every cell at that member, for every
-- metric. '' (the default): the metric's own format, as before.
ALTER TABLE model.dimension_def
    ADD COLUMN IF NOT EXISTS format_property text NOT NULL DEFAULT '';
