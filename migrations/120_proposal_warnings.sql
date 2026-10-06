-- The plan check's warnings on an AI proposal (formulas that run but look
-- wrong), kept with the proposal: they reached the console only in the
-- turn's stream and the saved tool message, so a reopened session showed the
-- proposal without them.

ALTER TABLE ai_assistant.proposal ADD COLUMN IF NOT EXISTS warnings TEXT[] NOT NULL DEFAULT '{}';
