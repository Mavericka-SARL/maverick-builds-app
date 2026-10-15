-- Personal API tokens: a credential a person creates for a script.
--
-- A script could load data only with a sign-in token copied from the
-- browser, which expires within the hour and cannot be renewed by the script
-- (a tester's bulk load stopped there, 2026-10-14). A personal token acts as
-- the person who made it — their roles, their access rules, their tenant —
-- until it expires or they revoke it, and is either read-only or read and
-- write.
--
-- The tokens live with the user directory in the control plane: a request
-- has to be resolved to a person before it can be routed to that person's
-- tenant database. Only a hash of each token is kept; the token itself is
-- shown once, when it is made.
CREATE TABLE IF NOT EXISTS platform.api_token (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    keycloak_sub TEXT NOT NULL,                 -- the person the token acts as
    name         TEXT NOT NULL CHECK (btrim(name) <> '' AND length(name) <= 100),
    token_hash   TEXT NOT NULL UNIQUE,          -- sha256 of the token, hex
    hint         TEXT NOT NULL,                 -- the token's first characters, to tell tokens apart
    access       TEXT NOT NULL CHECK (access IN ('read', 'write')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS api_token_subject ON platform.api_token (keycloak_sub) WHERE revoked_at IS NULL;
