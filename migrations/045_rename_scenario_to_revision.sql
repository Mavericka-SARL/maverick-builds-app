-- Consolidate "revision" semantics: model.scenario is the authoritative
-- working-revision concept (every metric def, dimension def, grid, dashboard,
-- and fact_input row carries a revision_id FK to it).  Rename it to
-- model.revision so the name matches its meaning.
--
-- Cascade:
--   model.revision (JSONB snapshot table from migration 018) → model.snapshot
--   model.scenario (working revision)                        → model.revision
--   core.revision  (structural schema version)               → core.schema_version
--   core.model.active_scenario (TEXT name cache)             → active_revision_name
--   runtime.fact_input.scenario (TEXT name cache)            → revision_name
--   runtime.calc_result.scenario (TEXT name cache)           → revision_name
--
-- All FK columns (revision_id) already have the correct name; only the
-- referenced table name changes.  PostgreSQL resolves FKs by OID, so
-- existing constraint definitions remain valid after the rename.

-- 1. Free the name 'revision' in the model schema
ALTER TABLE model.revision RENAME TO snapshot;

-- 2. Rename the working-revision table
ALTER TABLE model.scenario RENAME TO revision;

-- 3. Rename the schema-structural-version table
ALTER TABLE core.revision RENAME TO schema_version;

-- 4. Rename denormalised text caches
ALTER TABLE core.model          RENAME COLUMN active_scenario TO active_revision_name;
ALTER TABLE runtime.fact_input  RENAME COLUMN scenario        TO revision_name;
ALTER TABLE runtime.calc_result RENAME COLUMN scenario        TO revision_name;
