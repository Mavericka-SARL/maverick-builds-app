-- Input facts stored at a member code their dimension does not have. Until
-- 2026-10 the cell write, the gRPC write and form postings stored a value at
-- any code; no grid, chart or total reads such a row, it counts toward the
-- plan's fact rows and storage, and a member created later with that code
-- would have picked the value up without the write guard ever checking it.
-- Every write path now refuses an unknown code, so this runs once.
--
-- Only a key naming an existing dimension is judged: a row filed under a
-- deleted dimension, or under a key that is no dimension id, is left as it
-- is. A member's code change re-keys its facts (modeledit.RekeyMemberCode),
-- so a row at an unknown code is not a renamed member's. The archive trigger
-- (migration 081) keeps each deleted row in runtime.fact_input_history with
-- the reason 'orphan_member_code'.

DO $$
DECLARE
    removed bigint;
BEGIN
    PERFORM set_config('mvx.delete_reason', 'orphan_member_code', true);
    DELETE FROM runtime.fact_input f
    WHERE CASE WHEN jsonb_typeof(f.dim_members) = 'object' THEN EXISTS (
              SELECT 1
              FROM jsonb_each_text(f.dim_members) kv
              JOIN model.dimension_def d ON d.id::text = kv.key
              WHERE NOT EXISTS (SELECT 1 FROM model.dimension_member m
                                WHERE m.dimension_id = d.id AND m.code = kv.value))
          ELSE false END;
    GET DIAGNOSTICS removed = ROW_COUNT;
    PERFORM set_config('mvx.delete_reason', '', true);
    RAISE NOTICE 'migration 121: removed % input fact(s) at member codes their dimension does not have', removed;
END $$;
