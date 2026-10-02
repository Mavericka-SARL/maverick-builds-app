-- An AI Assistant session belongs to one model, not to its application.
-- Scoped to the application, a session followed whichever model a request
-- resolved: in an application with a second model the assistant answered
-- from the default model (the sign-up tour) while the Developer Console
-- showed the other one, and a proposal written in one model could be
-- confirmed into another.
--
-- Existing sessions take the model of their draft revision, else the
-- application's default model, else its newest — the order the gateway
-- resolved them by. A session whose application has no model left lost its
-- model with it, so it goes, as deleting a model now takes its sessions.

ALTER TABLE ai_assistant.session
    ADD COLUMN IF NOT EXISTS model_id UUID REFERENCES core.model(id) ON DELETE CASCADE;

UPDATE ai_assistant.session s
SET model_id = COALESCE(
    (SELECT rev.model_id FROM model.revision rev WHERE rev.id = s.draft_revision_id),
    (SELECT m.id FROM core.model m
      WHERE m.application_id = s.application_id
      ORDER BY (m.id = (SELECT default_model_id FROM core.application WHERE id = s.application_id)) IS TRUE DESC,
               m.created_at DESC, m.name, m.id
      LIMIT 1))
WHERE s.model_id IS NULL;

DELETE FROM ai_assistant.session WHERE model_id IS NULL;

ALTER TABLE ai_assistant.session ALTER COLUMN model_id SET NOT NULL;

CREATE INDEX IF NOT EXISTS session_model_user_idx ON ai_assistant.session (model_id, user_id);
