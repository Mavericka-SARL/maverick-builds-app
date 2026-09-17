-- model.snapshot (the JSONB point-in-time snapshot table introduced in migration 018)
-- is not referenced by any FK, not integrated into the live data model, and has no
-- callers in the application layer.  Drop it to eliminate the conceptual noise
-- alongside model.revision (the authoritative working-revision concept).
DROP TABLE IF EXISTS model.snapshot;
