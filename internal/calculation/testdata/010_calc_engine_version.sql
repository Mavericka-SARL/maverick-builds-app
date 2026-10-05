-- The calculation engine version the database's stored results were computed
-- with (calculation.EngineVersion). A change to the arithmetic bumps the
-- version; at start-up a database on an older one has every revision
-- recalculated in the background (calculation.RunEngineUpgrade). Found
-- 2026-10-05: after a fix to how a plain reference reads a dimension its
-- source lacks, a model kept showing its old rollup rows (210% for 2.5%)
-- until one of its inputs changed.
--
-- One row. claimed_at is a lease, so one replica runs the sweep and another
-- takes over if it dies.
CREATE TABLE IF NOT EXISTS runtime.calc_engine_state (
    id         BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    version    INTEGER NOT NULL DEFAULT 0,
    claimed_by TEXT,
    claimed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
INSERT INTO runtime.calc_engine_state (id, version) VALUES (true, 0) ON CONFLICT DO NOTHING;
