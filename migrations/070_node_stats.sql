-- Infrastructure node monitoring: a tiny DaemonSet on every k8s node
-- reports disk/memory/load into this table; the admin consoles read the
-- latest row per node. History is pruned by the collector itself (7 days).
CREATE SCHEMA IF NOT EXISTS ops;

CREATE TABLE IF NOT EXISTS ops.node_stat (
    id               BIGSERIAL PRIMARY KEY,
    node             TEXT NOT NULL,
    disk_total_gb    NUMERIC NOT NULL,
    disk_used_gb     NUMERIC NOT NULL,
    disk_pct         INT NOT NULL,
    mem_total_mb     INT NOT NULL,
    mem_available_mb INT NOT NULL,
    load1            NUMERIC NOT NULL DEFAULT 0,
    collected_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS node_stat_by_node ON ops.node_stat (node, collected_at DESC);
