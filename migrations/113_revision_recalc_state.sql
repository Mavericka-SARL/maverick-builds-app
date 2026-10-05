-- Recalculations a revision is waiting for. A write that answers before its
-- dependents are recalculated (POST /api/cells with "recalc": "background",
-- a business user removing a row) bumps requested; the pass that follows
-- raises done to the number it was started for. requested > done means the
-- grid's calculated cells are pending. A pass cut short by a restart leaves
-- the gap open, so readers only trust a gap younger than 15 minutes.
-- Found 2026-10-05: a numeric cell write on a 128-metric model took ~4 s,
-- and a removed row's contribution stayed in the totals for a minute with no
-- sign that a recalculation was running.
CREATE TABLE IF NOT EXISTS runtime.revision_recalc_state (
    revision_id UUID PRIMARY KEY,
    requested   BIGINT NOT NULL DEFAULT 0,
    done        BIGINT NOT NULL DEFAULT 0,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
