-- Usage analytics (enterprise feature `usage_analytics`): "active users"
-- needs to know who has been here. last_login_at is written by the identity
-- service's login upsert, which the HTTP path never touches; last_seen_at
-- is the gateway's own mark, refreshed at most every few minutes per
-- account so a busy session costs one write, not one per request.

ALTER TABLE identity.user ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS user_last_seen_idx ON identity.user (last_seen_at);
