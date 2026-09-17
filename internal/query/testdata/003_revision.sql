-- Minimal model.revision, needed by writeguard.CheckWrite's SystemManaged
-- check (Store.Writeback now runs the same generic write guard as HTTP
-- /api/cells and every import path).
CREATE TABLE model.revision (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id       UUID NOT NULL,
    system_managed BOOLEAN NOT NULL DEFAULT false
);
