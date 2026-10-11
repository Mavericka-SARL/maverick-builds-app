-- When a dedicated tenant last had something happen in it: a request, a
-- platform administrator acting on it, or background work that found work.
-- Background loops in every process read it to sleep while a tenant is quiet
-- instead of polling its database every few seconds (pkg/tenantdb Waker).
-- NULL until the first activity after this migration.
ALTER TABLE platform.tenant_database ADD COLUMN IF NOT EXISTS last_active_at TIMESTAMPTZ;
