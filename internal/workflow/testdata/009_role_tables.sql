-- Mirror of the identity.role_assignment/business_role/business_role_member
-- pieces of migrations/002_identity.sql and 020_business_roles_access.sql,
-- needed for Store.IsAssigneeEligible's assignee_roles eligibility check.
-- core.application also gains customer_id (nullable), mirroring the real
-- schema's flatten-hierarchy shape, since IsAssigneeEligible's business-role
-- lookup falls back to it when workspace_id doesn't match.

CREATE TYPE identity.user_role AS ENUM (
    'platform_admin',
    'developer',
    'business_admin',
    'business_user'
);

CREATE TABLE identity.role_assignment (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    role         identity.user_role NOT NULL,
    workspace_id UUID REFERENCES core.workspace(id) ON DELETE CASCADE,
    assigned_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, role, workspace_id)
);

CREATE TABLE identity.business_role (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES core.workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (workspace_id, name)
);

CREATE TABLE identity.business_role_member (
    role_id UUID NOT NULL REFERENCES identity.business_role(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES identity.user(id)          ON DELETE CASCADE,
    PRIMARY KEY (role_id, user_id)
);

ALTER TABLE core.application
    ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id);
