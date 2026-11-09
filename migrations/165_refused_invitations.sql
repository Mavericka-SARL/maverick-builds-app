-- 165: an invitation the platform refuses is listed like a new one
-- (owner's decision, 2026-11-08). Inviting a platform admin's or a
-- platform-wide builder's address is refused, answered as a success, and
-- written to the platform's audit only — but the tenant's users list then
-- lacked the address, which a new one would show, so a tenant admin could
-- test addresses for the platform's own administrators. Each refusal leaves
-- a row here, which the tenant's list shows as a pending person with
-- nothing granted, and which deleting that person removes.
CREATE TABLE IF NOT EXISTS identity.refused_invitation (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    customer_id  UUID NOT NULL REFERENCES core.customer(id) ON DELETE CASCADE,
    email        TEXT NOT NULL,
    display_name TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX IF NOT EXISTS refused_invitation_tenant_email_uq
    ON identity.refused_invitation (customer_id, lower(email));
