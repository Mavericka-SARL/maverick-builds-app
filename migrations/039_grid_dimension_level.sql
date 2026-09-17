-- display_level controls which depth of a dimension's hierarchy to show in a grid.
-- NULL  = show all levels (full hierarchy with multi-level headers)
-- 0     = root members only (each aggregates its entire subtree)
-- N > 0 = members at exactly depth N
-- -1    = leaf members only (most granular, all editable)
ALTER TABLE model.grid_dimension
    ADD COLUMN IF NOT EXISTS display_level INT;
