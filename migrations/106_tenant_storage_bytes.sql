-- The bytes a tenant's data occupies, as the plan sweep last measured them
-- (internal/plan: exact in a dedicated database, an estimate in a shared one).
-- Recorded beside usage_checked_at so a tenant admin can see the space used
-- and, on a plan with a storage limit, the space left — before the workspace
-- turns read-only, not only after. NULL until the first sweep.
ALTER TABLE core.customer ADD COLUMN IF NOT EXISTS storage_bytes BIGINT;
