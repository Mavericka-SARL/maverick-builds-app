-- Whether people of a tenant may change grid data from a chat connection
-- (ChatGPT, Claude: internal/gateway/mcp.go). On by default: a connection
-- writes only what its person may write in the console, and only once they
-- granted it the write scope. A tenant administrator turns it off for the
-- whole tenant (internal/gateway/connector_settings.go); reads stay.
ALTER TABLE core.customer
    ADD COLUMN IF NOT EXISTS chat_writes BOOL NOT NULL DEFAULT true;
