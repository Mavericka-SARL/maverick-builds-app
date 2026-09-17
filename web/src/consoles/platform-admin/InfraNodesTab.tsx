import { useQuery } from "@tanstack/react-query";
import { AlertTriangle } from "lucide-react";
import { api, type InfraNode } from "../../api/client";
import { LoadingState, EmptyState, StatusBadge } from "../../ui";

// Infrastructure health for tenant/platform admins: latest disk/memory/load
// per cluster node from the node-stats DaemonSet. Built after two CI
// disk-full incidents showed node disks had no eyes on them at all.

function Bar({ pct, warnAt, dangerAt }: { pct: number; warnAt: number; dangerAt: number }) {
  const color = pct >= dangerAt ? "var(--color-danger)" : pct >= warnAt ? "var(--color-warning)" : "var(--color-success)";
  return (
    <div style={{ background: "var(--color-surface-muted)", borderRadius: 4, height: 8, overflow: "hidden" }}>
      <div style={{ width: `${Math.min(100, Math.max(0, pct))}%`, height: "100%", background: color }} />
    </div>
  );
}

function ago(iso: string): string {
  const d = new Date(iso.replace(" ", "T"));
  if (isNaN(d.getTime())) return iso;
  const mins = Math.round((Date.now() - d.getTime()) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins} min ago`;
  return `${Math.round(mins / 60)} h ago`;
}

export function InfraNodesTab() {
  const { data: nodes = [], isLoading } = useQuery({
    queryKey: ["infra-nodes"],
    queryFn: api.getInfraNodes,
    refetchInterval: 60_000,
  });

  if (isLoading) return <LoadingState label="Loading node health…" />;
  if (!nodes.length) {
    return <EmptyState label="No node samples yet — the node-stats collector reports every 5 minutes." />;
  }

  return (
    <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(300px, 1fr))", gap: 12 }}>
      {nodes.map((n: InfraNode) => {
        const memPct = n.mem_total_mb > 0 ? Math.round(((n.mem_total_mb - n.mem_available_mb) / n.mem_total_mb) * 100) : 0;
        return (
          <div key={n.node} className="mvx-panel" style={{ padding: 16, display: "flex", flexDirection: "column", gap: 10 }}>
            <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
              <strong style={{ fontSize: 14 }}>{n.node}</strong>
              {n.stale ? (
                <StatusBadge tone="danger">
                  <AlertTriangle size={11} style={{ marginRight: 3, verticalAlign: -1 }} />
                  stale
                </StatusBadge>
              ) : n.disk_pct >= 90 || memPct >= 90 ? (
                <StatusBadge tone="danger">critical</StatusBadge>
              ) : n.disk_pct >= 80 || memPct >= 80 ? (
                <StatusBadge tone="warning">warning</StatusBadge>
              ) : (
                <StatusBadge tone="success">healthy</StatusBadge>
              )}
              <span className="mvx-admin-muted" style={{ marginLeft: "auto", fontSize: 11 }}>{ago(n.collected_at)}</span>
            </div>
            <div>
              <div style={{ display: "flex", justifyContent: "space-between", fontSize: 12, marginBottom: 3 }}>
                <span className="mvx-admin-muted">Disk</span>
                <span>{n.disk_used_gb.toFixed(0)} / {n.disk_total_gb.toFixed(0)} GB · {n.disk_pct}%</span>
              </div>
              <Bar pct={n.disk_pct} warnAt={80} dangerAt={90} />
            </div>
            <div>
              <div style={{ display: "flex", justifyContent: "space-between", fontSize: 12, marginBottom: 3 }}>
                <span className="mvx-admin-muted">Memory</span>
                <span>{((n.mem_total_mb - n.mem_available_mb) / 1024).toFixed(1)} / {(n.mem_total_mb / 1024).toFixed(1)} GB · {memPct}%</span>
              </div>
              <Bar pct={memPct} warnAt={80} dangerAt={90} />
            </div>
            <div className="mvx-admin-muted" style={{ fontSize: 12 }}>Load (1m): {n.load1.toFixed(2)}</div>
            {n.stale && (
              <div style={{ fontSize: 12, color: "var(--color-danger)" }}>
                No sample for over 15 minutes — the node or its collector may be down.
              </div>
            )}
          </div>
        );
      })}
    </div>
  );
}
