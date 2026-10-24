-- An import job belongs to the revision it wrote into: deleting the revision
-- deletes its jobs, and with them their staged rows and errors (their own
-- keys cascade from the job).
--
-- Seen 2026-10-23 recording a how-to video on the Playground: a deleted
-- revision's two imports were still listed in Recent Imports, on another
-- revision's Integrations page, with Revision "—". revision_id had no
-- foreign key since it was added (037), so no delete reached them. The jobs
-- left by revisions already deleted go with this migration.
DELETE FROM import.import_job j
WHERE j.revision_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM model.revision r WHERE r.id = j.revision_id);

-- A revision's delete finds its jobs by this index, not a scan.
CREATE INDEX IF NOT EXISTS import_job_revision_id_idx ON import.import_job (revision_id);

ALTER TABLE import.import_job
    ADD CONSTRAINT import_job_revision_id_fkey
    FOREIGN KEY (revision_id) REFERENCES model.revision(id) ON DELETE CASCADE;
