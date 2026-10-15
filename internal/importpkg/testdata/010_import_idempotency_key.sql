-- An import sent again is not applied twice.
--
-- A client that sends POST /api/import/upload with an Idempotency-Key header
-- and does not get the answer (the connection closed, a 502 from the
-- ingress, a gateway restarted) cannot tell whether the import was saved.
-- Sent again, an incremental import added every value a second time. The
-- key a job was sent with is kept on the job: the same person sending the
-- same key for the same model gets the first import's answer instead.
ALTER TABLE import.import_job ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS import_job_idempotency_key
    ON import.import_job (model_id, created_by, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
