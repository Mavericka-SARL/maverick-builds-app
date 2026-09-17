-- Drop deployment.deployment_job and deployment.migration_run: both backed
-- the gRPC deployment builder (internal/deployment/server.go +
-- cmd/deployment), which had zero callers anywhere in this codebase — no
-- frontend, no other service dialed it — and no real gRPC authentication to
-- protect it (BuildExportPackage performed no tenant-ownership check at
-- all). deployment_job.revision_id had also silently drifted to reference
-- core.schema_version instead of model.revision after migration 045
-- renamed core.revision -> core.schema_version (Postgres FKs follow by
-- OID through a rename), masked because nothing ever passed a non-empty
-- revision_id. Standalone packaging now ships as one synchronous HTTP
-- route (GET /api/admin/models/{id}/export/package) built on
-- internal/modeltransfer, the same tested code the existing JSON export
-- endpoint already used — no async job/status model needed.
--
-- deployment.migration_run (a migration-application log keyed by
-- model_id/revision_id) is unrelated to deployment.schema_migration (the
-- live table internal/schemamigration actually uses for per-model
-- reporting-view generation) and was never written to by any Go code.

DROP TABLE IF EXISTS deployment.deployment_job;
DROP TABLE IF EXISTS deployment.migration_run;
DROP TYPE IF EXISTS deployment.deployment_status;
