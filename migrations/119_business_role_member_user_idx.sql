-- Business role memberships by person. The primary key is (role_id,
-- user_id), so every lookup of one person's roles — the role checks, the
-- workflow assignee resolution, a user's delete cascading here — read the
-- whole table. Found in the 2026-10 observations; not measured on a large
-- tenant.

CREATE INDEX IF NOT EXISTS business_role_member_user_idx ON identity.business_role_member (user_id);
