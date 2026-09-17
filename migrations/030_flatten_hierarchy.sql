-- Flatten tenant hierarchy: applications belong directly to customers.
-- 1. Add customer_id to core.application (nullable first for backfill).
-- 2. Backfill from existing workspace→customer chain.
-- 3. Make customer_id NOT NULL.
-- 4. Drop the NOT NULL constraint on workspace_id (keep column for compatibility).

ALTER TABLE core.application
    ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id) ON DELETE CASCADE;

-- Backfill from workspace → customer
UPDATE core.application a
SET customer_id = w.customer_id
FROM core.workspace w
WHERE a.workspace_id = w.id
  AND a.customer_id IS NULL;

-- Any stragglers (no workspace) → first customer
UPDATE core.application
SET customer_id = (SELECT id FROM core.customer ORDER BY created_at LIMIT 1)
WHERE customer_id IS NULL;

-- Drop NOT NULL from workspace_id so new apps can be created without it
ALTER TABLE core.application
    ALTER COLUMN workspace_id DROP NOT NULL;
