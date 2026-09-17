-- Generic rollup support: a read-only grid can mirror another grid's
-- metrics, computing cells via cross-dimension rollup against its own
-- dimensions instead of owning grid_metric rows itself. Keeps each
-- metric's grid_metric membership single and unambiguous (grid()'s
-- metricDims is computed per metric across the whole revision) while
-- letting the same metric appear, rolled up, in any number of summary
-- grids.
ALTER TABLE model.grid_def
    ADD COLUMN IF NOT EXISTS rollup_source_grid_id UUID REFERENCES model.grid_def(id) ON DELETE SET NULL;

-- Property-based dimension grouping: the structural counterpart
-- (parent_dimension_id, migration 052) only allows one parent per
-- dimension. Real models often need a second, orthogonal grouping (e.g.
-- employees grouped by cost center AND by region) — this lets a dimension
-- declare its members are derived from another dimension's member
-- property instead of a structural parent link.
ALTER TABLE model.dimension_def
    ADD COLUMN IF NOT EXISTS source_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS source_property TEXT;

-- A revision only ever populated by an approval-triggered copy (see the
-- workflow step "on_approve" config) is never directly writable through
-- the normal cell writeback endpoint.
ALTER TABLE model.revision
    ADD COLUMN IF NOT EXISTS system_managed BOOLEAN NOT NULL DEFAULT false;
