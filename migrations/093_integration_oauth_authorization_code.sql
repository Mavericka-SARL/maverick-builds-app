-- OAuth 2.0 authorization code for REST connector connections (2026-09-21;
-- the flow 073 reserved and deferred). A connection of auth_type
-- 'oauth2_authorization_code' is connected once by a developer, who
-- consents at the provider on the tenant's behalf; the tokens then belong
-- to the connection, so scheduled runs use them like any other credential
-- (sealed in secret_enc with client_secret, access_token, refresh_token,
-- expires_at). The consent round trip leaves the console and comes back to
-- the gateway's public callback, so the pending authorisation is a row —
-- one per click, short-lived, bound to the connection and the person who
-- started it — not memory, which a second gateway replica would not share.
CREATE TABLE IF NOT EXISTS model.integration_oauth_state (
    state          TEXT PRIMARY KEY,
    connection_id  UUID NOT NULL REFERENCES model.integration_connection(id) ON DELETE CASCADE,
    application_id UUID NOT NULL,
    user_id        UUID,
    -- PKCE: the verifier stays here, the S256 challenge goes to the provider.
    code_verifier  TEXT NOT NULL,
    -- Where the console is sent after the callback.
    return_to      TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL
);
