-- Add tenant_admin to user_role enum.
-- tenant_admin manages their own customer's hierarchy (workspaces, apps, users)
-- but cannot access cross-tenant platform data.
ALTER TYPE identity.user_role ADD VALUE IF NOT EXISTS 'tenant_admin';
