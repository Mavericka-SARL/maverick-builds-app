-- Minimal schema for identity package integration tests.

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE SCHEMA IF NOT EXISTS core;
CREATE SCHEMA IF NOT EXISTS identity;

CREATE OR REPLACE FUNCTION core.set_updated_at()
RETURNS TRIGGER AS $$
BEGIN NEW.updated_at = now(); RETURN NEW; END;
$$ LANGUAGE plpgsql;

CREATE TABLE core.customer (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT NOT NULL,
    plan       TEXT NOT NULL DEFAULT 'starter',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TYPE identity.user_role AS ENUM (
    'platform_admin', 'developer', 'business_admin', 'business_user', 'tenant_admin'
);

CREATE TABLE identity.user (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    keycloak_sub  TEXT NOT NULL UNIQUE,
    email         TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    customer_id   UUID REFERENCES core.customer(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ
);

CREATE TABLE identity.role_assignment (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    role         identity.user_role NOT NULL,
    workspace_id UUID,
    assigned_by  UUID,
    assigned_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, role, workspace_id)
);

CREATE TABLE identity.api_key (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id      UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    key_hash     TEXT NOT NULL UNIQUE,
    expires_at   TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ
);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON identity.user
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
