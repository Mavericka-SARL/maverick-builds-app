-- A calculated member: a member of a standard dimension whose value, for
-- every metric, is computed from the dimension's other members at the same
-- coordinate — Variance = {RF} - {LY} on a Scenario dimension — instead of
-- being entered or rolled up. NULL is an ordinary member. Calculated
-- members are left out of every aggregation, and nothing is stored at them.
-- Before this, a comparison layout (RF, LY, Var, Var %) of a 13-line P&L
-- took 52 metrics on four grids.
ALTER TABLE model.dimension_member ADD COLUMN IF NOT EXISTS formula TEXT;

-- A calculated member is computed from its siblings, never a total of
-- children and never part of one: it is a top-level member with no members
-- under it. Members are written by a dozen paths (console, AI
-- Developer, imports, integrations, model import), so the rule is kept here.
CREATE OR REPLACE FUNCTION model.calculated_member_guard() RETURNS trigger AS $$
DECLARE
    parent_code TEXT;
BEGIN
    IF NEW.parent_member_id IS NOT NULL THEN
        SELECT p.code INTO parent_code FROM model.dimension_member p
        WHERE p.id = NEW.parent_member_id AND NULLIF(btrim(p.formula), '') IS NOT NULL;
        IF FOUND THEN
            RAISE EXCEPTION 'member % cannot go under % — % is a calculated member, computed from its siblings, and has no members under it',
                NEW.code, parent_code, parent_code USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    IF NULLIF(btrim(NEW.formula), '') IS NOT NULL AND EXISTS (
        SELECT 1 FROM model.dimension_member c WHERE c.parent_member_id = NEW.id) THEN
        RAISE EXCEPTION 'member % has members under it, so it cannot be a calculated member', NEW.code
            USING ERRCODE = 'check_violation';
    END IF;
    IF NULLIF(btrim(NEW.formula), '') IS NOT NULL AND NEW.parent_member_id IS NOT NULL THEN
        RAISE EXCEPTION 'member % is under another member, so it cannot be a calculated member — a calculated member is a top-level member, in no total',
            NEW.code USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS dimension_member_calculated_guard ON model.dimension_member;
CREATE TRIGGER dimension_member_calculated_guard
    BEFORE INSERT OR UPDATE OF parent_member_id, formula ON model.dimension_member
    FOR EACH ROW EXECUTE FUNCTION model.calculated_member_guard();
