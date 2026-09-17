-- Two additions this package's curated schema needs to exercise test runs.
--
-- 1. workflow_instance.test_run (production migration 065). A test run is a
--    property of the instance, so every side-effect site can consult it
--    without threading a mode through the call graph.
--
-- 2. notification.notification (production migrations 011 + 061, at the
--    columns Store.Send actually writes). Without the table, a notification
--    step's dispatch fails silently and a test asserting "a test run sends
--    nothing" would pass for the wrong reason — it has to be able to observe
--    a REAL run sending something.

ALTER TABLE workflow.workflow_instance
    ADD COLUMN IF NOT EXISTS test_run BOOLEAN NOT NULL DEFAULT false;

CREATE SCHEMA IF NOT EXISTS notification;

DO $$ BEGIN
    CREATE TYPE notification.channel AS ENUM ('in_app', 'email', 'webhook');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

DO $$ BEGIN
    CREATE TYPE notification.notif_status AS ENUM ('pending', 'delivered', 'failed', 'read');
EXCEPTION WHEN duplicate_object THEN NULL; END $$;

CREATE TABLE IF NOT EXISTS notification.notification (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient_user_id UUID NOT NULL REFERENCES identity."user"(id) ON DELETE CASCADE,
    channel           notification.channel NOT NULL DEFAULT 'in_app',
    template_id       TEXT NOT NULL,
    template_vars     JSONB NOT NULL DEFAULT '{}',
    status            notification.notif_status NOT NULL DEFAULT 'pending',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at      TIMESTAMPTZ,
    resource_type     TEXT,
    resource_id       TEXT
);
