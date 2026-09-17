-- Lets a notification point back at the object it's about, so the
-- Business Console notification center (and its siblings in the other
-- consoles) can navigate there. Mirrors audit_event's existing
-- resource_type/resource_id design: polymorphic, no FK, since resource_type
-- varies by producer.

ALTER TABLE notification.notification
    ADD COLUMN IF NOT EXISTS resource_type TEXT,
    ADD COLUMN IF NOT EXISTS resource_id   TEXT;

CREATE INDEX IF NOT EXISTS notification_resource_idx
    ON notification.notification (resource_type, resource_id);
