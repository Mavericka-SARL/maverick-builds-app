-- The image widget: a picture on a dashboard, kept inline in the content
-- column as a base64 data URL (internal/imagedata) so it travels with the
-- dashboard through a revision copy, an export and an import rather than
-- living in a second store that has to be kept in step.
--
-- Only the documented list of widget types changes; no column does. It is a
-- migration of its own rather than an edit to 043 because a database that
-- has already run 043 would never see the new text.
COMMENT ON COLUMN model.dashboard_widget.widget_type IS
    'grid | form | automation_button | integration_button | text | image | workflow_action';
