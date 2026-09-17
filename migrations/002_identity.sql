-- Schema: identity
-- Users, role assignments, sessions, API keys

CREATE SCHEMA IF NOT EXISTS identity;

CREATE TYPE identity.user_role AS ENUM (
    'platform_admin',
    'developer',
    'business_admin',
    'business_user'
);

CREATE TABLE identity.user (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    keycloak_sub    TEXT NOT NULL UNIQUE,  -- Keycloak subject claim
    email           TEXT NOT NULL UNIQUE,
    display_name    TEXT NOT NULL DEFAULT '',
    customer_id     UUID REFERENCES core.customer(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at   TIMESTAMPTZ
);

CREATE INDEX ON identity.user (customer_id);
CREATE INDEX ON identity.user (keycloak_sub);

-- A user may have different roles in different workspaces.
-- customer_id-level roles (platform_admin, developer) have NULL workspace_id.
CREATE TABLE identity.role_assignment (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    role         identity.user_role NOT NULL,
    workspace_id UUID REFERENCES core.workspace(id) ON DELETE CASCADE,
    assigned_by  UUID REFERENCES identity.user(id) ON DELETE SET NULL,
    assigned_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, role, workspace_id)
);

CREATE INDEX ON identity.role_assignment (user_id);
CREATE INDEX ON identity.role_assignment (workspace_id);

CREATE TABLE identity.api_key (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id     UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    key_hash    TEXT NOT NULL UNIQUE,   -- bcrypt hash of the raw key
    expires_at  TIMESTAMPTZ,
    revoked_at  TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX ON identity.api_key (user_id);
CREATE INDEX ON identity.api_key (key_hash);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON identity.user
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
