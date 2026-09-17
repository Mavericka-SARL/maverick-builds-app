-- Enterprise identity: single sign-on through a tenant's own identity
-- provider (licence feature `sso`) and SCIM 2.0 provisioning (`scim`).
--
-- The tenant's IdP secret (an OIDC client secret, a SAML certificate) is
-- registered in Keycloak and never stored here: this row is what the console
-- shows and what first-login provisioning checks. One row per tenant
-- database, like notification.settings and ai_assistant.tenant_llm_settings.

CREATE TABLE IF NOT EXISTS identity.sso_provider (
    id               BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    -- The Keycloak identity-provider alias: mvx-<customer id without dashes>.
    -- Empty until the tenant registers a provider.
    alias            TEXT        NOT NULL DEFAULT '',
    protocol         TEXT        NOT NULL DEFAULT 'oidc' CHECK (protocol IN ('oidc', 'saml')),
    display_name     TEXT        NOT NULL DEFAULT '',
    -- What the tenant admin typed: an OIDC discovery URL or a SAML metadata
    -- URL, kept so the screen can show it and re-import it.
    metadata_url     TEXT        NOT NULL DEFAULT '',
    -- The OIDC client id at the tenant's IdP (SAML: the IdP entity id),
    -- for display only.
    client_id        TEXT        NOT NULL DEFAULT '',
    -- E-mail domains this provider may assert. A brokered login whose
    -- address is outside the list is refused: an IdP can assert any e-mail
    -- it likes, and this is what keeps it to its own people.
    allowed_domains  TEXT[]      NOT NULL DEFAULT '{}',
    -- Create the account on first sign-in (the alternative is SCIM or an
    -- invitation first).
    jit_provisioning BOOLEAN     NOT NULL DEFAULT TRUE,
    default_role     identity.user_role NOT NULL DEFAULT 'business_user',
    enabled          BOOLEAN     NOT NULL DEFAULT FALSE,
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO identity.sso_provider (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING;

-- Control-plane index from e-mail domain to the tenant that owns it, so the
-- sign-in page can route a person to their provider before anyone knows who
-- they are. Written whenever a tenant saves its provider. With dedicated
-- databases this lives in the control plane; in shared mode it is simply
-- another table.
CREATE TABLE IF NOT EXISTS platform.sso_domain (
    domain      TEXT PRIMARY KEY,
    customer_id UUID NOT NULL,
    alias       TEXT NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS sso_domain_customer_idx ON platform.sso_domain (customer_id);

-- SCIM bearer tokens. The token itself is shown once and stored hashed; its
-- prefix carries the customer id so a request can be routed to the right
-- database before the hash is checked. created_by is not a foreign key on
-- purpose: the issuing actor may be a platform admin who has no row in
-- this tenant's database.
CREATE TABLE IF NOT EXISTS identity.scim_token (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name         TEXT        NOT NULL,
    token_hash   TEXT        NOT NULL UNIQUE,
    -- What a provisioned user gets, and where: the platform role and the
    -- workspace groups (business roles) are created in.
    default_role identity.user_role NOT NULL DEFAULT 'business_user',
    workspace_id UUID REFERENCES core.workspace(id) ON DELETE SET NULL,
    created_by   UUID,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_used_at TIMESTAMPTZ,
    revoked_at   TIMESTAMPTZ
);

-- Deactivation. SCIM deprovisions by setting active=false long before it
-- deletes; a disabled user keeps their rows and history but is refused at
-- sign-in and by every actor resolver. external_id is the IdP's own
-- identifier for the person (SCIM externalId), which is how Entra and Okta
-- find a user again after a rename.
ALTER TABLE identity.user
    ADD COLUMN IF NOT EXISTS disabled_at  TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS external_id  TEXT,
    ADD COLUMN IF NOT EXISTS scim_managed BOOLEAN NOT NULL DEFAULT FALSE;
CREATE INDEX IF NOT EXISTS user_external_id_idx ON identity.user (external_id) WHERE external_id IS NOT NULL;

-- A SCIM group maps to a business role; the group's externalId lets the
-- IdP rename the group without losing the mapping.
ALTER TABLE identity.business_role
    ADD COLUMN IF NOT EXISTS external_id TEXT;
