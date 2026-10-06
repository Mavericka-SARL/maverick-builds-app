-- The model a self-service tenant lands on is now the Developer guide, whose
-- first page builds a first model; it was the "Learn the platform" tour
-- (starter.LandingKey, 2026-10-06). Sign-up and starter sync set it for new
-- tenants; this moves the existing ones, once. Only an application whose
-- default is still the tour sign-up gave it moves, and only to the same
-- tenant's Developer guide in that same application: a default someone
-- changed under Build › Models is theirs and stays, and a deleted guide
-- (its starter_model.model_id set to NULL) is not brought back.
UPDATE core.application a
SET default_model_id = guide.model_id
FROM core.starter_model tour
JOIN core.starter_model guide
  ON guide.customer_id = tour.customer_id AND guide.starter_key = 'developer_guide'
JOIN core.model m ON m.id = guide.model_id
WHERE tour.starter_key = 'tour'
  AND a.default_model_id = tour.model_id
  AND m.application_id = a.id;
