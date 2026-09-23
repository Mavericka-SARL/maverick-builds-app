-- Per-tenant settings (2026-09-20 parity audit).
--
-- Notification delivery (078), audit retention (082), the tenant AI key
-- (079) and the SSO provider (080) were one row per DATABASE: right for a
-- dedicated tenant database, wrong for a shared one, where tenant admin A's
-- webhook received tenant B's notifications, A's AI key paid for B's calls
-- and A's identity provider signed everyone in. SCIM tokens carried the
-- tenant inside the token but not on the row, so every admin saw every
-- token.
--
-- From here on each of these is keyed by customer_id. NULL is the
-- deployment's own row — the defaults a tenant inherits until it sets its
-- own, an enterprise capability (pkg/license: deployment_settings) for
-- delivery, retention and the AI key; the identity tables have no
-- deployment row, because a provider or a token without a tenant has
-- nowhere to put the people it signs in.
--
-- Existing data keeps meaning what it meant: the one row becomes the
-- deployment row AND is copied to every tenant of this database, so a
-- shared deployment behaves exactly as before the upgrade (each tenant now
-- owns its copy) and a dedicated database ends up with its tenant's row.
-- The SSO row goes to the tenant its alias names — the alias is
-- mvx-<customer id> — and SCIM tokens to the tenant of their workspace, or
-- to the database's only tenant.

-- ── notification.settings ────────────────────────────────────────────────
ALTER TABLE notification.settings ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id) ON DELETE CASCADE;
ALTER TABLE notification.settings DROP CONSTRAINT IF EXISTS settings_pkey;
ALTER TABLE notification.settings DROP COLUMN IF EXISTS id;
CREATE UNIQUE INDEX IF NOT EXISTS notification_settings_customer_idx
    ON notification.settings (customer_id) NULLS NOT DISTINCT;
INSERT INTO notification.settings (customer_id, email_enabled, webhook_enabled, webhook_url, webhook_secret, reminders_enabled, reminder_lead_hours)
SELECT c.id, s.email_enabled, s.webhook_enabled, s.webhook_url, s.webhook_secret, s.reminders_enabled, s.reminder_lead_hours
FROM core.customer c CROSS JOIN notification.settings s
WHERE s.customer_id IS NULL
ON CONFLICT (customer_id) DO NOTHING;
-- The deployment row always exists, so a read never has to create it.
INSERT INTO notification.settings (customer_id) VALUES (NULL) ON CONFLICT (customer_id) DO NOTHING;

-- ── audit.settings ───────────────────────────────────────────────────────
ALTER TABLE audit.settings ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id) ON DELETE CASCADE;
ALTER TABLE audit.settings DROP CONSTRAINT IF EXISTS settings_pkey;
ALTER TABLE audit.settings DROP COLUMN IF EXISTS id;
CREATE UNIQUE INDEX IF NOT EXISTS audit_settings_customer_idx
    ON audit.settings (customer_id) NULLS NOT DISTINCT;
INSERT INTO audit.settings (customer_id, retention_days)
SELECT c.id, s.retention_days FROM core.customer c CROSS JOIN audit.settings s WHERE s.customer_id IS NULL
ON CONFLICT (customer_id) DO NOTHING;
INSERT INTO audit.settings (customer_id) VALUES (NULL) ON CONFLICT (customer_id) DO NOTHING;

-- ── ai_assistant.tenant_llm_settings ─────────────────────────────────────
ALTER TABLE ai_assistant.tenant_llm_settings ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id) ON DELETE CASCADE;
ALTER TABLE ai_assistant.tenant_llm_settings DROP CONSTRAINT IF EXISTS tenant_llm_settings_pkey;
ALTER TABLE ai_assistant.tenant_llm_settings DROP COLUMN IF EXISTS id;
CREATE UNIQUE INDEX IF NOT EXISTS tenant_llm_settings_customer_idx
    ON ai_assistant.tenant_llm_settings (customer_id) NULLS NOT DISTINCT;
INSERT INTO ai_assistant.tenant_llm_settings (customer_id, provider, model, api_key_enc, enforced)
SELECT c.id, s.provider, s.model, s.api_key_enc, s.enforced
FROM core.customer c CROSS JOIN ai_assistant.tenant_llm_settings s WHERE s.customer_id IS NULL
ON CONFLICT (customer_id) DO NOTHING;
INSERT INTO ai_assistant.tenant_llm_settings (customer_id) VALUES (NULL) ON CONFLICT (customer_id) DO NOTHING;

-- ── identity.sso_provider ────────────────────────────────────────────────
ALTER TABLE identity.sso_provider ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id) ON DELETE CASCADE;
ALTER TABLE identity.sso_provider DROP CONSTRAINT IF EXISTS sso_provider_pkey;
ALTER TABLE identity.sso_provider DROP COLUMN IF EXISTS id;
CREATE UNIQUE INDEX IF NOT EXISTS sso_provider_customer_idx
    ON identity.sso_provider (customer_id) NULLS NOT DISTINCT;
-- A registered provider belongs to the tenant its alias names.
UPDATE identity.sso_provider s SET customer_id = c.id
FROM core.customer c
WHERE s.customer_id IS NULL AND s.alias <> '' AND s.alias = 'mvx-' || replace(c.id::text, '-', '');
-- Whatever is left is an empty singleton (or an alias of a tenant that is
-- not in this database): no deployment row for identity.
DELETE FROM identity.sso_provider WHERE customer_id IS NULL;

-- ── identity.scim_token ──────────────────────────────────────────────────
ALTER TABLE identity.scim_token ADD COLUMN IF NOT EXISTS customer_id UUID REFERENCES core.customer(id) ON DELETE CASCADE;
UPDATE identity.scim_token t SET customer_id = w.customer_id
FROM core.workspace w WHERE t.customer_id IS NULL AND t.workspace_id = w.id;
UPDATE identity.scim_token SET customer_id = (SELECT id FROM core.customer)
WHERE customer_id IS NULL AND (SELECT count(*) FROM core.customer) = 1;
CREATE INDEX IF NOT EXISTS scim_token_customer_idx ON identity.scim_token (customer_id);
