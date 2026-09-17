-- White-labelling (feature `white_label`, commercial and enterprise): a
-- tenant's own name, tagline, logo, favicon and colour for the console,
-- and the name its outbound e-mail carries. Keyed by customer, not one row
-- per database: on a shared database a per-database row would be one brand
-- for every tenant, and a brand is the most visible thing a tenant owns.
-- Images are stored inline as data URLs (small by rule) so branding needs
-- no object store and arrives in one request before anyone is signed in.

CREATE TABLE IF NOT EXISTS core.branding (
    customer_id      UUID PRIMARY KEY,
    product_name     TEXT        NOT NULL DEFAULT '',
    tagline          TEXT        NOT NULL DEFAULT '',
    logo_data_url    TEXT        NOT NULL DEFAULT '',
    favicon_data_url TEXT        NOT NULL DEFAULT '',
    brand_color      TEXT        NOT NULL DEFAULT '',
    email_from_name  TEXT        NOT NULL DEFAULT '',
    custom_domain    TEXT        NOT NULL DEFAULT '',
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Control-plane index from a custom host to its tenant, so the console can
-- show the right brand before sign-in. The host itself (DNS, TLS, ingress)
-- is the operator's to set up; this only says whose it is.
CREATE TABLE IF NOT EXISTS platform.branding_domain (
    domain      TEXT PRIMARY KEY,
    customer_id UUID NOT NULL,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
