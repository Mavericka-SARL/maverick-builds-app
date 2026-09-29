-- Metric names are unique regardless of case within a revision (and among a
-- model's revision-less metrics). Formulas read metrics by name without
-- regard to case — the evaluator binds values by upper-cased name and the
-- formula save resolves names the same way — so two metrics whose names
-- differ only in case ("Sales" beside "sales") would be ambiguous.
--
-- Enforced by unique indexes on lower(name). Their violation is SQLSTATE
-- 23505, which the writers answer as METRIC_NAME_TAKEN, as for the
-- exact-name indexes metric_def_null_rev_uq / metric_def_with_rev_uq
-- (migration 027). Production and staging held no such pair when this was
-- written (checked 2026-09-29).
--
-- A database that already holds a pair cannot build those indexes, and a
-- failed migration would stop it starting. There the same rule is enforced
-- by the trigger below instead — every new name and every rename checked,
-- also raising 23505, with a transaction-scoped advisory lock on (model,
-- revision, lower(name)) so the second of two concurrent writers waits for
-- the first and then sees its row — and the migration raises a WARNING. Once
-- the pair is renamed, dropping the trigger and building the indexes with the
-- statements of the first branch finishes the job.
CREATE OR REPLACE FUNCTION model.metric_def_name_ci_check() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM pg_advisory_xact_lock(hashtextextended(
    'metric_def:' || NEW.model_id::text || ':' || COALESCE(NEW.revision_id::text, '') || ':' || lower(NEW.name), 0));
  IF EXISTS (
    SELECT 1 FROM model.metric_def m
    WHERE m.model_id = NEW.model_id
      AND m.revision_id IS NOT DISTINCT FROM NEW.revision_id
      AND lower(m.name) = lower(NEW.name)
      AND m.id <> NEW.id
  ) THEN
    RAISE EXCEPTION 'metric name "%" differs only in case from an existing metric of the revision', NEW.name
      USING ERRCODE = 'unique_violation', CONSTRAINT = 'metric_def_name_ci_check';
  END IF;
  RETURN NEW;
END
$$;

DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM model.metric_def
    GROUP BY model_id, revision_id, lower(name) HAVING count(*) > 1
  ) THEN
    CREATE UNIQUE INDEX IF NOT EXISTS metric_def_name_ci_uq
      ON model.metric_def (model_id, revision_id, lower(name)) WHERE revision_id IS NOT NULL;
    CREATE UNIQUE INDEX IF NOT EXISTS metric_def_null_rev_name_ci_uq
      ON model.metric_def (model_id, lower(name)) WHERE revision_id IS NULL;
  ELSE
    RAISE WARNING 'model.metric_def holds metric names that differ only in case within one revision; '
      'enforcing unique names by trigger until they are renamed (see migration 100)';
    DROP TRIGGER IF EXISTS metric_def_name_ci_check ON model.metric_def;
    CREATE TRIGGER metric_def_name_ci_check
      BEFORE INSERT OR UPDATE OF name, model_id, revision_id ON model.metric_def
      FOR EACH ROW EXECUTE FUNCTION model.metric_def_name_ci_check();
  END IF;
END
$$;
