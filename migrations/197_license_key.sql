-- The license key a platform administrator installs under Platform › License
-- (owner, 2026-11-22), so a Commercial or Enterprise key needs no access to
-- the server's configuration and no restart. One row per deployment, on the
-- control plane. A key in MAVERICKS_LICENSE_KEY or MAVERICKS_LICENSE_FILE
-- still wins over it.
--
-- token is the key as pasted, through internal/secretbox: encrypted
-- ("enc:v1:") when SECRETS_ENCRYPTION_KEY is set, as AI keys are. The
-- license id, edition and expiry beside it are copies for listings and
-- audit; the gateway trusts only the signed token.

CREATE TABLE IF NOT EXISTS platform.license_key (
    one          BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (one),
    token        TEXT NOT NULL,
    license_id   TEXT NOT NULL,
    edition      TEXT NOT NULL,
    expires_at   TIMESTAMPTZ NOT NULL,
    installed_by UUID,
    installed_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
