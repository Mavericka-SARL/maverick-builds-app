-- The Community limit note (129) ends "… or get the Commercial or Enterprise
-- edition."; every other word people read says "Edition", capitalised, as
-- the terms do.
-- Only 129's seeded wording is replaced: a note a platform administrator has
-- written stays theirs.
UPDATE platform.plan
SET limit_note = 'A Community workspace holds up to 100 MB. To go further, run the platform on your own infrastructure — free for your company''s internal use — or get the Commercial or Enterprise Edition.',
    updated_at = now()
WHERE key = 'community'
  AND limit_note = 'A Community workspace holds up to 100 MB. To go further, run the platform on your own infrastructure — free for your company''s internal use — or get the Commercial or Enterprise edition.';
