-- Least-privilege roles per schema module.
-- Each service gets a login user with only the grants it needs.
-- Group roles (NOLOGIN) aggregate schema privileges; service users inherit them.

-- ── Group roles (idempotent via DO block) ──────────────────────────────────────

DO $$
DECLARE r TEXT;
BEGIN
  FOREACH r IN ARRAY ARRAY[
    'role_core','role_identity','role_security','role_model','role_runtime',
    'role_workflow','role_import','role_audit','role_deployment',
    'role_notification'
  ] LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = r) THEN
      EXECUTE format('CREATE ROLE %I NOLOGIN', r);
    END IF;
  END LOOP;
END
$$;

-- ── Schema USAGE grants ────────────────────────────────────────────────────────

GRANT USAGE ON SCHEMA core          TO role_core;
GRANT USAGE ON SCHEMA identity      TO role_identity;
GRANT USAGE ON SCHEMA security      TO role_security;
GRANT USAGE ON SCHEMA model         TO role_model;
GRANT USAGE ON SCHEMA runtime       TO role_runtime;
GRANT USAGE ON SCHEMA workflow      TO role_workflow;
GRANT USAGE ON SCHEMA import        TO role_import;
GRANT USAGE ON SCHEMA audit         TO role_audit;
GRANT USAGE ON SCHEMA deployment    TO role_deployment;
GRANT USAGE ON SCHEMA notification  TO role_notification;

-- ── Table-level grants ─────────────────────────────────────────────────────────

GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA core          TO role_core;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA identity      TO role_identity;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA security      TO role_security;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA model         TO role_model;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA runtime       TO role_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA workflow      TO role_workflow;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA import        TO role_import;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA deployment    TO role_deployment;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA notification  TO role_notification;

-- audit is append-only: SELECT for compliance queries, INSERT only (no UPDATE/DELETE)
GRANT SELECT, INSERT                 ON ALL TABLES IN SCHEMA audit         TO role_audit;

-- ── Sequence grants ────────────────────────────────────────────────────────────

GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA core          TO role_core;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA identity      TO role_identity;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA security      TO role_security;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA model         TO role_model;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA runtime       TO role_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA workflow      TO role_workflow;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA import        TO role_import;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA audit         TO role_audit;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA deployment    TO role_deployment;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA notification  TO role_notification;

-- ── Default privileges (apply to future tables) ────────────────────────────────

ALTER DEFAULT PRIVILEGES IN SCHEMA core          GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_core;
ALTER DEFAULT PRIVILEGES IN SCHEMA identity      GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_identity;
ALTER DEFAULT PRIVILEGES IN SCHEMA security      GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_security;
ALTER DEFAULT PRIVILEGES IN SCHEMA model         GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_model;
ALTER DEFAULT PRIVILEGES IN SCHEMA runtime       GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA workflow      GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_workflow;
ALTER DEFAULT PRIVILEGES IN SCHEMA import        GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_import;
ALTER DEFAULT PRIVILEGES IN SCHEMA audit         GRANT SELECT, INSERT                 ON TABLES    TO role_audit;
ALTER DEFAULT PRIVILEGES IN SCHEMA deployment    GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_deployment;
ALTER DEFAULT PRIVILEGES IN SCHEMA notification  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES    TO role_notification;

ALTER DEFAULT PRIVILEGES IN SCHEMA core          GRANT USAGE, SELECT ON SEQUENCES TO role_core;
ALTER DEFAULT PRIVILEGES IN SCHEMA identity      GRANT USAGE, SELECT ON SEQUENCES TO role_identity;
ALTER DEFAULT PRIVILEGES IN SCHEMA model         GRANT USAGE, SELECT ON SEQUENCES TO role_model;
ALTER DEFAULT PRIVILEGES IN SCHEMA runtime       GRANT USAGE, SELECT ON SEQUENCES TO role_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA workflow      GRANT USAGE, SELECT ON SEQUENCES TO role_workflow;
ALTER DEFAULT PRIVILEGES IN SCHEMA import        GRANT USAGE, SELECT ON SEQUENCES TO role_import;
ALTER DEFAULT PRIVILEGES IN SCHEMA notification  GRANT USAGE, SELECT ON SEQUENCES TO role_notification;

-- ── Service login users (idempotent) ──────────────────────────────────────────
-- Passwords are overridden via environment variables in production (Vault / K8s Secret).

DO $$
DECLARE u TEXT;
BEGIN
  FOREACH u IN ARRAY ARRAY[
    'svc_gateway','svc_identity','svc_tenant','svc_model','svc_schema_migration',
    'svc_policy','svc_calculation','svc_workflow','svc_import','svc_query',
    'svc_audit','svc_notification','svc_deployment'
  ] LOOP
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = u) THEN
      EXECUTE format('CREATE USER %I PASSWORD %L', u, u || '_dev');
    END IF;
  END LOOP;
END
$$;

GRANT role_core, role_identity, role_security, role_audit TO svc_gateway;
GRANT role_identity, role_core, role_audit TO svc_identity;
GRANT role_core, role_identity, role_audit TO svc_tenant;
GRANT role_model, role_core, role_runtime, role_audit TO svc_model;
GRANT role_model, role_deployment, role_audit TO svc_schema_migration;
GRANT CREATE ON DATABASE mavericks TO svc_schema_migration;
GRANT role_security, role_identity, role_core, role_audit TO svc_policy;
GRANT role_runtime, role_model, role_audit TO svc_calculation;
GRANT role_workflow, role_core, role_identity, role_notification, role_audit TO svc_workflow;
GRANT role_import, role_runtime, role_model, role_audit TO svc_import;
GRANT role_runtime, role_model, role_core, role_security TO svc_query;
GRANT role_audit TO svc_audit;
GRANT role_notification, role_audit TO svc_notification;
GRANT role_deployment, role_model, role_audit TO svc_deployment;
