-- The model a self-service tenant lands on is the "Learn the platform" tour
-- again, whose first page links each reader on to the guide for what they
-- came to do; since migration 122 it was the Developer guide
-- (starter.LandingKey, 2026-10-07). Sign-up and starter sync set it for new
-- tenants; this moves the existing ones, once. Only an application whose
-- default is still the Developer guide sign-up or 122 gave it moves, and
-- only to the same tenant's tour in that same application: a default
-- someone changed under Developer › Models is theirs and stays, and a
-- deleted tour (its starter_model.model_id set to NULL) is not brought back.
UPDATE core.application a
SET default_model_id = tour.model_id
FROM core.starter_model guide
JOIN core.starter_model tour
  ON tour.customer_id = guide.customer_id AND tour.starter_key = 'tour'
JOIN core.model m ON m.id = tour.model_id
WHERE guide.starter_key = 'developer_guide'
  AND a.default_model_id = guide.model_id
  AND m.application_id = a.id;
