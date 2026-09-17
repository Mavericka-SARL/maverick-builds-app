-- Adds application_id to audit.audit_event so non-revision-scoped mutations
-- (tenant/application/user/role management — the biggest audit coverage gap)
-- can still be attributed to an application. revision_id (added by migration
-- 027) never got the FK every other revision-scoped column added since then
-- has had; adding it here too.

ALTER TABLE audit.audit_event
    ADD COLUMN IF NOT EXISTS application_id UUID REFERENCES core.application(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS audit_event_application_id_idx
    ON audit.audit_event (application_id, occurred_at);

-- Defensive cleanup before adding the FK below: audit_event.revision_id has
-- had no FK since it was added (migration 027), so a row could in principle
-- reference a since-deleted revision.
UPDATE audit.audit_event SET revision_id = NULL
WHERE revision_id IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM model.revision r WHERE r.id = audit_event.revision_id);

-- Postgres (through at least v16) does not support NOT VALID foreign keys
-- on a partitioned table when the partitioned table is the referencing
-- side, so this validates immediately rather than the usual
-- NOT VALID + VALIDATE CONSTRAINT two-step.
ALTER TABLE audit.audit_event
    ADD CONSTRAINT audit_event_revision_id_fkey
        FOREIGN KEY (revision_id) REFERENCES model.revision(id) ON DELETE SET NULL;
