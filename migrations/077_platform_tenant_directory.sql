-- Dedicated database per tenant (pkg/tenantdb). These tables live in the
-- control-plane database — the one DATABASE_URL points at — and describe
-- which tenants have their own database, where their users and applications
-- are, so a request can be routed before any tenant data is read. The same
-- migration also runs inside every tenant database (they carry the full
-- schema); there the tables simply stay empty.
CREATE SCHEMA IF NOT EXISTS platform;

CREATE TABLE IF NOT EXISTS platform.tenant_database (
    customer_id UUID PRIMARY KEY,            -- equals core.customer.id inside the tenant database
    name        TEXT NOT NULL,               -- catalog copy, so listings need not open every database
    plan        TEXT NOT NULL DEFAULT 'starter',
    database    TEXT NOT NULL UNIQUE,        -- PostgreSQL database name, e.g. tenant_<uuid hex>
    status      TEXT NOT NULL CHECK (status IN ('provisioning', 'ready', 'failed', 'disabled')),
    error       TEXT NOT NULL DEFAULT '',    -- why status is failed
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Which dedicated tenants an identity (Keycloak subject) has a user row in.
CREATE TABLE IF NOT EXISTS platform.user_directory (
    keycloak_sub TEXT NOT NULL,
    customer_id  UUID NOT NULL REFERENCES platform.tenant_database(customer_id) ON DELETE CASCADE,
    email        TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (keycloak_sub, customer_id)
);
CREATE INDEX IF NOT EXISTS user_directory_customer_idx ON platform.user_directory (customer_id);

-- Which dedicated tenant holds an application, so a request that only
-- carries X-App-Id can be routed.
CREATE TABLE IF NOT EXISTS platform.application_directory (
    application_id UUID PRIMARY KEY,
    customer_id    UUID NOT NULL REFERENCES platform.tenant_database(customer_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS application_directory_customer_idx ON platform.application_directory (customer_id);
