-- Tenant-level AI provider key (enterprise feature `tenant_ai_keys`).
--
-- Community and commercial deployments keep the per-user key in
-- ai_assistant.llm_settings, which this does not touch. An enterprise tenant
-- can instead hold ONE key, managed by its tenant admin, so developers never
-- paste a personal key and the tenant's AI spend sits on one account. When
-- `enforced` is set the tenant key is the only key used: personal keys are
-- ignored, which is the point for a customer who must not have model data
-- leave through an employee's private provider account.
--
-- One row per tenant database, exactly like notification.settings.

CREATE TABLE IF NOT EXISTS ai_assistant.tenant_llm_settings (
    id          BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    provider    TEXT        NOT NULL DEFAULT 'openai',
    -- Empty means "the provider's default model", resolved in Go so the
    -- default can change with the provider catalog without a migration.
    model       TEXT        NOT NULL DEFAULT '',
    -- AES-256-GCM under AI_KEY_ENCRYPTION_SECRET, same helper as the
    -- per-user key. Empty means no tenant key is configured.
    api_key_enc TEXT        NOT NULL DEFAULT '',
    -- When true, personal keys are ignored and every AI call in this tenant
    -- uses the key above.
    enforced    BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO ai_assistant.tenant_llm_settings (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING;

GRANT SELECT, INSERT, UPDATE, DELETE ON ai_assistant.tenant_llm_settings TO role_ai_assistant;
