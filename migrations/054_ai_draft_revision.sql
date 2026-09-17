-- AI chat sessions write into an isolated draft revision instead of the
-- live active revision. draft_revision_id is set lazily on the session's
-- first confirmed proposal (see aiConfirmProposal); NULL means the session
-- hasn't written anything yet and still reads the active revision.
--
-- ON DELETE SET NULL: discarding a draft (DELETE /api/developer/revisions/{id})
-- must not be blocked by, or cascade into, the chat session that created it.
ALTER TABLE ai_assistant.session
    ADD COLUMN IF NOT EXISTS draft_revision_id UUID REFERENCES model.revision(id) ON DELETE SET NULL;
