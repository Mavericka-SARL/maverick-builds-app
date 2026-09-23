-- Platform-level role grants could be duplicated.
--
-- identity.role_assignment has UNIQUE (user_id, role, workspace_id), and every
-- writer relies on it with ON CONFLICT DO NOTHING. PostgreSQL treats NULLs as
-- distinct in a unique constraint, so that guard only ever worked for
-- workspace-scoped grants: granting the same PLATFORM-level role twice (the
-- Users panel, SCIM's default role, a first single-sign-on login, the
-- bootstrap script) inserted a second identical row.
--
-- The rows are not merely untidy. actorByKeycloakSub builds an actor's roles
-- with string_agg over this table, so GET /api/me answered
-- {"roles": ["platform_admin", "platform_admin", ...]} for such an account
-- (seen on staging, 2026-09-17, after the bootstrap script ran four times).
-- The console happens to de-duplicate; nothing else does.
--
-- A partial unique index covers exactly the case the constraint cannot, and
-- makes every existing ON CONFLICT DO NOTHING (no conflict target, so any
-- unique index arbitrates) behave as its author intended.

-- Keep the earliest grant of each pair; assigned_by/assigned_at of the later
-- duplicates carry no information the first row lacks.
WITH ranked AS (
    SELECT id, row_number() OVER (PARTITION BY user_id, role ORDER BY assigned_at, id) AS rn
    FROM identity.role_assignment
    WHERE workspace_id IS NULL
)
DELETE FROM identity.role_assignment ra
USING ranked
WHERE ra.id = ranked.id AND ranked.rn > 1;

CREATE UNIQUE INDEX IF NOT EXISTS role_assignment_platform_level_uniq
    ON identity.role_assignment (user_id, role)
    WHERE workspace_id IS NULL;
