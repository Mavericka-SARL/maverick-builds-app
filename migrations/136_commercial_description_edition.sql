-- The Commercial Edition's description (085, seeded for the old "standard"
-- plan) still says "A paid plan."; every word people read says edition.
-- Only the seeded wording is replaced: a description a platform
-- administrator has written stays theirs.
UPDATE platform.plan
SET description = 'A paid edition. Set its limits here.', updated_at = now()
WHERE key = 'commercial' AND description = 'A paid plan. Set its limits here.';
