-- An application and a model each carry a short description, written by the
-- person who creates them (a tenant or platform administrator) and shown
-- wherever the application or model is chosen, so the people using them
-- know what each is for. Empty until someone writes one.

ALTER TABLE core.application ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
ALTER TABLE core.model       ADD COLUMN IF NOT EXISTS description TEXT NOT NULL DEFAULT '';
