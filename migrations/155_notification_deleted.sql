-- A person's bell can be cleared, one notification or all. The row stays,
-- marked: My workflow history finds a run through the notification that told
-- its recipient of it, the access-granted notice counts what it already sent,
-- and mail still queued goes out; a person clearing the bell changes none of
-- these. Only the bell's list leaves marked rows out.
ALTER TABLE notification.notification ADD COLUMN IF NOT EXISTS deleted_at timestamptz;
