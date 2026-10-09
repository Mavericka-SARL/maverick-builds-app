-- A model link's owner: the developer of both models who created it, or who
-- last changed what it reads or activated it. Every run of the link reads
-- its source as the owner — a developer's Run now, its schedule, a business
-- user's dashboard button — so a person with no access to the source model
-- can still refresh the link's data, and the link stops when its owner is
-- no longer a developer of both models (until a developer of both activates
-- it again and becomes its owner). NULL on a link saved before this column:
-- such a link reads as the person who runs it, as it did.
--
-- No foreign key, as source_switched_by: a platform administrator need not
-- be a row of a dedicated tenant's identity.user.
ALTER TABLE model.integration_def
    ADD COLUMN IF NOT EXISTS link_owner UUID;
