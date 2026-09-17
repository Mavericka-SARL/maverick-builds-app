-- Schema: notification
-- In-app, email, and webhook notification records

CREATE SCHEMA IF NOT EXISTS notification;

CREATE TYPE notification.channel AS ENUM ('in_app', 'email', 'webhook');
CREATE TYPE notification.notif_status AS ENUM ('pending', 'delivered', 'failed', 'read');

CREATE TABLE notification.notification (
    id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    recipient_user_id UUID NOT NULL REFERENCES identity.user(id) ON DELETE CASCADE,
    channel           notification.channel NOT NULL DEFAULT 'in_app',
    template_id       TEXT NOT NULL,
    template_vars     JSONB NOT NULL DEFAULT '{}',
    status            notification.notif_status NOT NULL DEFAULT 'pending',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    delivered_at      TIMESTAMPTZ
);

CREATE INDEX ON notification.notification (recipient_user_id, status);
CREATE INDEX ON notification.notification (created_at DESC);
