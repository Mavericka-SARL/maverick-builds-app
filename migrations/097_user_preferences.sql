-- Per-person preferences: display choices a signed-in person makes for
-- themselves, starting with the console theme (light / dark / system).
--
-- A JSON object of known keys only. The gateway validates every key and
-- value against its registry (internal/gateway/preferences.go) before
-- writing, so the column never holds anything the console does not
-- understand. It lives on the user row, so it goes when the user does.

ALTER TABLE identity.user ADD COLUMN IF NOT EXISTS preferences JSONB NOT NULL DEFAULT '{}'::jsonb;
