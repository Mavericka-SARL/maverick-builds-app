-- Schema: security
-- RACI rules, dimension-member policies, metric policies, cell policies, ABAC rules

CREATE SCHEMA IF NOT EXISTS security;

CREATE TYPE security.raci_type AS ENUM ('responsible', 'accountable', 'consulted', 'informed');
CREATE TYPE security.policy_action AS ENUM ('read', 'write', 'approve', 'admin');

-- RACI assignments: applies only to business_user role
CREATE TABLE security.raci_rule (
    id               UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id   UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    user_id          UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    resource_pattern TEXT NOT NULL,   -- glob pattern matching workflow/step/resource IDs
    raci_type        security.raci_type NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, user_id, resource_pattern, raci_type)
);

CREATE INDEX ON security.raci_rule (application_id, user_id);

-- Dimension-member policy: restricts which members a user can see/edit
CREATE TABLE security.dimension_member_policy (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id       UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    user_id              UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    dimension_id         UUID NOT NULL,   -- FK to model.dimension_def (created in migration 004)
    allowed_member_codes TEXT[] NOT NULL DEFAULT '{}',
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, user_id, dimension_id)
);

CREATE INDEX ON security.dimension_member_policy (application_id, user_id);

-- Metric policy: controls read/write access per metric per user
CREATE TABLE security.metric_policy (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    user_id        UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    metric_id      UUID NOT NULL,   -- FK to model.metric_def (created in migration 004)
    can_read       BOOLEAN NOT NULL DEFAULT true,
    can_write      BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (application_id, user_id, metric_id)
);

CREATE INDEX ON security.metric_policy (application_id, user_id);

-- Cell policy: individual writeback cell read/write control for business users
CREATE TABLE security.cell_policy (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    user_id        UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    metric_id      UUID NOT NULL,
    dim_members    JSONB NOT NULL,   -- {dimension_id: member_code, ...}
    can_read       BOOLEAN NOT NULL DEFAULT true,
    can_write      BOOLEAN NOT NULL DEFAULT false,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON security.cell_policy (application_id, user_id);
CREATE INDEX ON security.cell_policy USING GIN (dim_members);

-- ABAC rules: attribute-based expressions evaluated at request time
CREATE TABLE security.abac_rule (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    application_id UUID NOT NULL REFERENCES core.application(id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    expression     TEXT NOT NULL,   -- boolean expression string, evaluated by policy engine
    actions        security.policy_action[] NOT NULL,
    resource_type  TEXT NOT NULL,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX ON security.abac_rule (application_id);

CREATE TRIGGER set_updated_at BEFORE UPDATE ON security.dimension_member_policy
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON security.metric_policy
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
CREATE TRIGGER set_updated_at BEFORE UPDATE ON security.abac_rule
    FOR EACH ROW EXECUTE FUNCTION core.set_updated_at();
