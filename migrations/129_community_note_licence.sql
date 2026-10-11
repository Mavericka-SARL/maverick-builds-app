-- The Community plan's limit note (089, renamed by 096 and 123) told a tenant
-- that self-hosting is free "for non-commercial use, under a commercial
-- licence otherwise". The Sustainable Use License (LICENSE) makes it free for
-- a company's internal use, commercial companies included, so the note
-- misstated the licence.
-- Only the seeded wording is replaced: a note a platform administrator has
-- written stays theirs.
UPDATE platform.plan
SET limit_note = 'A Community workspace holds up to 100 MB. To go further, run the platform on your own infrastructure — free for your company''s internal use — or get the Commercial or Enterprise edition.',
    updated_at = now()
WHERE key = 'community'
  AND limit_note = 'A Community workspace holds up to 100 MB. To go further, run the platform on your own infrastructure — free of charge and without limits for non-commercial use, under a commercial licence otherwise — or get the enterprise edition.';
