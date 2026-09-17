-- A dimension can declare another dimension as its parent (e.g. Cabinet is a
-- child of Department). dimension_member.parent_member_id already has no
-- same-dimension constraint at the DB level, so it can point cross-dimension
-- once the owning dimension declares parent_dimension_id — enforcement is
-- application-level (see handler.go / write_executor.go validation).
ALTER TABLE model.dimension_def
    ADD COLUMN IF NOT EXISTS parent_dimension_id UUID REFERENCES model.dimension_def(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS dimension_def_parent_dimension_idx ON model.dimension_def (parent_dimension_id);
