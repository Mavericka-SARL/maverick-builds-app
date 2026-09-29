-- Dimension names are unique regardless of case within a revision (and among
-- a model's revision-less dimensions), as metric names are since migration
-- 100. Formulas match dimension names without regard to case — the save's
-- revisionDims.lookup and the evaluator's Dim.Current — so "Region" beside
-- "region" would leave a formula naming either reading whichever it met
-- first.
--
-- Enforced as in migration 100: unique indexes on lower(name), whose
-- violation (SQLSTATE 23505) the writers answer as DIMENSION_NAME_TAKEN, as
-- for the exact-name indexes dimension_def_null_rev_uq /
-- dimension_def_with_rev_uq (migration 027); production and staging held no
-- such pair (checked 2026-09-29). A database that already holds one gets the
-- trigger below instead (every new name and rename checked, 23505, serialised
-- per name by an advisory lock) and a WARNING, so it keeps starting.
CREATE OR REPLACE FUNCTION model.dimension_def_name_ci_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended(
    'dimension_def:' || NEW.model_id::text || ':' || COALESCE(NEW.revision_id::text, '') || ':' || lower(NEW.name), 0));
  IF EXISTS (
    SELECT 1 FROM model.dimension_def d
    WHERE d.model_id = NEW.model_id
      AND d.revision_id IS NOT DISTINCT FROM NEW.revision_id
      AND lower(d.name) = lower(NEW.name)
      AND d.id <> NEW.id
  ) THEN
    RAISE EXCEPTION 'dimension name "%" differs only in case from an existing dimension of the revision', NEW.name
      USING ERRCODE = 'unique_violation', CONSTRAINT = 'dimension_def_name_ci_check';
  END IF;
  RETURN NEW;
END
$$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM model.dimension_def
    GROUP BY model_id, revision_id, lower(name) HAVING count(*) > 1
  ) THEN
    CREATE UNIQUE INDEX IF NOT EXISTS dimension_def_name_ci_uq
      ON model.dimension_def (model_id, revision_id, lower(name)) WHERE revision_id IS NOT NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS dimension_def_null_rev_name_ci_uq
      ON model.dimension_def (model_id, lower(name)) WHERE revision_id IS NULL;
  ELSE
    RAISE WARNING 'model.dimension_def holds dimension names that differ only in case within one revision; '
      'enforcing unique names by trigger until they are renamed (see migration 101)';
    DROP TRIGGER IF EXISTS dimension_def_name_ci_check ON model.dimension_def;
    CREATE TRIGGER dimension_def_name_ci_check
      BEFORE INSERT OR UPDATE OF name, model_id, revision_id ON model.dimension_def
      FOR EACH ROW EXECUTE FUNCTION model.dimension_def_name_ci_check();
  END IF;
END
$$;
