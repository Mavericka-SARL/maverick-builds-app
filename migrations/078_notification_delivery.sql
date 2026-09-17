-- Outbound notification delivery: e-mail, webhooks and task reminders.
--
-- Until now every notification was written straight to 'delivered' and only
-- ever read in the console, because in_app was the only channel with anything
-- behind it. Rows on the other two channels would have sat pending forever.
-- These columns give the dispatcher (internal/notification) somewhere to
-- record attempts and back off, and the settings table says which channels a
-- tenant actually wants.

ALTER TABLE notification.notification
    ADD COLUMN IF NOT EXISTS attempts        INT         NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_error      TEXT        NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- The dispatcher's claim query: pending, off the in_app channel, and due.
CREATE INDEX IF NOT EXISTS notification_outbound_due_idx
    ON notification.notification (next_attempt_at)
    WHERE status = 'pending' AND channel <> 'in_app';

-- One row per database (a tenant, or the whole deployment in shared mode).
-- SMTP credentials deliberately live in the deployment's environment, not
-- here: they belong to whoever runs the server, and a tenant admin should not
-- be able to point the mail relay somewhere else.
CREATE TABLE IF NOT EXISTS notification.settings (
    id                  BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    email_enabled       BOOLEAN     NOT NULL DEFAULT FALSE,
    webhook_enabled     BOOLEAN     NOT NULL DEFAULT FALSE,
    webhook_url         TEXT        NOT NULL DEFAULT '',
    -- Shared secret for the X-Mavericks-Signature header, so a receiver can
    -- verify a delivery really came from this deployment.
    webhook_secret      TEXT        NOT NULL DEFAULT '',
    reminders_enabled   BOOLEAN     NOT NULL DEFAULT FALSE,
    -- How long before a task's due time to remind its assignees; 0 means at
    -- the due time itself.
    reminder_lead_hours INT         NOT NULL DEFAULT 0 CHECK (reminder_lead_hours >= 0),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO notification.settings (id) VALUES (TRUE) ON CONFLICT (id) DO NOTHING;

-- Reminders are sent once per step; this records when, so a step that stays
-- overdue is not re-sent on every pass.
ALTER TABLE workflow.workflow_step
    ADD COLUMN IF NOT EXISTS reminded_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS workflow_step_reminder_idx
    ON workflow.workflow_step (due_at)
    WHERE status = 'in_progress' AND reminded_at IS NULL;
