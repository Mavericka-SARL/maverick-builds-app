-- The self-service plan is called "Basic workspace" (089 seeded it as "Test
-- workspace"). Only the seeded wording is replaced: a deployment whose
-- platform administrators renamed the plan or rewrote its texts in
-- Platform › Plans keeps theirs. The key stays 'test' — sign-up, tenants and
-- the enforcer refer to the plan by it, and it is never shown.
UPDATE platform.plan SET name = 'Basic workspace'
 WHERE key = 'test' AND name = 'Test workspace';

UPDATE platform.plan SET description = 'Use the platform for as long as you like, with up to 100 MB of data.'
 WHERE key = 'test' AND description = 'Try the platform for as long as you like, with up to 100 MB of data.';

UPDATE platform.plan SET limit_note = replace(limit_note, 'A test workspace holds', 'A basic workspace holds')
 WHERE key = 'test' AND limit_note LIKE 'A test workspace holds%';
