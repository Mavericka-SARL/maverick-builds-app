-- 172: the stored deployment packages go (owner-approved, 2026-11-09).
--
-- The standalone deployment package export was removed on 2026-10-12. Each
-- row here stands for a packages/<model>/<revision>.tar.gz object in the
-- gateway's bucket: a copy of a model's definitions and, by default, its
-- entered values, which deleting the model, its revision or its tenant never
-- removed and which nothing reads any more. The rows go here, in every
-- database; the objects themselves are removed from the bucket by the
-- operator (docs/SELF_HOSTING.md, "Upgrading").
DELETE FROM storage.object WHERE owner_type = 'deployment_package';
