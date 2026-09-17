-- Dimension hierarchy: allow members to have a parent member
ALTER TABLE model.dimension_member
    ADD COLUMN IF NOT EXISTS parent_member_id UUID REFERENCES model.dimension_member(id) ON DELETE SET NULL;

-- Aggregation rule per dimension (how totals/subtotals are rolled up)
ALTER TABLE model.dimension_def
    ADD COLUMN IF NOT EXISTS agg_rule TEXT NOT NULL DEFAULT 'sum';

-- Model revisions: snapshots of model state (like git commits)
CREATE TABLE IF NOT EXISTS model.revision (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    model_id     UUID NOT NULL REFERENCES core.model(id) ON DELETE CASCADE,
    message      TEXT NOT NULL,
    snapshot     JSONB NOT NULL DEFAULT '{}',
    created_by   TEXT NOT NULL DEFAULT 'system',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
