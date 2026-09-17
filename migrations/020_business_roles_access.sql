-- Business roles: named groups owned by a business admin within a workspace.
-- Roles define which dashboards members can see.
CREATE TABLE identity.business_role (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES core.workspace(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(workspace_id, name)
);

-- Dashboards visible to users who hold a given business role
CREATE TABLE identity.business_role_dashboard (
    role_id      UUID NOT NULL REFERENCES identity.business_role(id) ON DELETE CASCADE,
    dashboard_id UUID NOT NULL REFERENCES model.dashboard_def(id) ON DELETE CASCADE,
    PRIMARY KEY (role_id, dashboard_id)
);

-- Users assigned to a business role
CREATE TABLE identity.business_role_member (
    role_id  UUID NOT NULL REFERENCES identity.business_role(id) ON DELETE CASCADE,
    user_id  UUID NOT NULL REFERENCES identity.user(id)          ON DELETE CASCADE,
    PRIMARY KEY (role_id, user_id)
);

-- Per-user granular access overrides (dimension / metric / button)
-- access: write = full edit, read = view only, hidden = not visible
CREATE TABLE identity.user_access_rule (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id    UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    rule_type  TEXT NOT NULL CHECK (rule_type IN ('dimension_member', 'metric', 'button')),
    ref_id     TEXT NOT NULL,
    access     TEXT NOT NULL DEFAULT 'write' CHECK (access IN ('write', 'read', 'hidden')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(user_id, rule_type, ref_id)
);
