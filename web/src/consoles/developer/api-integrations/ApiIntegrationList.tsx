import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Play, Pencil, Copy, History, Power, Trash2 } from "lucide-react";
import { api, type ApiIntegrationDetail, type ApiRun, type IntegrationDef } from "../../../api/client";
import { EmptyState, IconButton, LoadingState, StatusBadge, useConfirm } from "../../../ui";
import { ApiRunHistory } from "./ApiRunHistory";

// Saved connector cards: status, method, sanitized host, direction, target,
// revision, schedule, tags, last-run — with Run now / Edit / Duplicate /
// History / Enable-Disable / Delete.
export function ApiIntegrationList({
  revisionId, onEdit,
}: {
  revisionId?: string;
  onEdit: (d: ApiIntegrationDetail) => void;
}) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [notice, setNotice] = useState<string | null>(null);
  const [historyFor, setHistoryFor] = useState<string | null>(null);

  // The shared integrations listing, filtered to rest_api CLIENT-SIDE by
  // type (the legacy sections filter the same list to their own types —
  // rest_api no longer leaks into the CSV section and vice versa).
  const { data: all = [], isPending } = useQuery({
    queryKey: ["dev-integrations", revisionId, "rest_api"],
    queryFn: () => api.listIntegrations(revisionId),
  });
  const items = (all as IntegrationDef[]).filter(i => i.type === "rest_api");

  const detail = useMutation({
    mutationFn: (id: string) => api.getApiIntegration(id),
  });

  const invalidate = () => qc.invalidateQueries({ queryKey: ["dev-integrations", revisionId, "rest_api"] });

  const runNow = async (id: string) => {
    try {
      const r = (await api.runIntegration(id)) as unknown as { run_id?: string };
      setNotice(`Run queued${r.run_id ? ` (${r.run_id.slice(0, 8)}…)` : ""}`);
      setHistoryFor(id);
    } catch (e) {
      setNotice((e as Error).message);
    }
  };

  if (isPending) return <LoadingState label="Loading integrations…" />;
  if (items.length === 0) return <EmptyState label="No REST API integrations yet — create one above." />;

  return (
    <div style={{ display: "grid", gap: 12 }}>
      {confirmElement}
      {notice && <p role="status" aria-live="polite" className="mvx-admin-muted" style={{ margin: 0, fontSize: 12 }}>{notice}</p>}
      {items.map(item => (
        <ApiCard key={item.id} item={item} revisionId={revisionId}
          onRun={() => runNow(item.id)}
          onEdit={async () => onEdit(await detail.mutateAsync(item.id))}
          onDuplicate={async () => { await api.duplicateApiIntegration(item.id); invalidate(); }}
          onToggleHistory={() => setHistoryFor(h => (h === item.id ? null : item.id))}
          historyOpen={historyFor === item.id}
          onToggleEnabled={async (enabled) => { await api.updateApiIntegration(item.id, { enabled }); invalidate(); }}
          onDelete={() => confirm({
            title: `Delete “${item.name}”?`,
            body: "The definition, its schedule and run history are removed. Dashboard buttons referencing it are removed too.",
            confirmLabel: "Delete",
            destructive: true,
            onConfirm: async () => { await api.deleteIntegration(item.id); invalidate(); },
          })}
        />
      ))}
    </div>
  );
}

function ApiCard({
  item, revisionId, onRun, onEdit, onDuplicate, onToggleHistory, historyOpen, onToggleEnabled, onDelete,
}: {
  item: IntegrationDef; revisionId?: string;
  onRun: () => void; onEdit: () => void; onDuplicate: () => void;
  onToggleHistory: () => void; historyOpen: boolean;
  onToggleEnabled: (enabled: boolean) => void; onDelete: () => void;
}) {
  // The card enriches itself from the typed detail + last run lazily.
  const { data: det } = useQuery({
    queryKey: ["api-integration", item.id],
    queryFn: () => api.getApiIntegration(item.id),
  });
  const { data: runs } = useQuery({
    queryKey: ["api-integration-runs", item.id, "last"],
    queryFn: () => api.listApiIntegrationRuns(item.id),
  });
  const last: ApiRun | undefined = runs?.[0];
  const cfg = det?.config;
  const host = cfg ? hostOf(cfg.request.url) : "";
  const sched = det?.schedule;

  return (
    <div className="mvx-admin-object" style={{ padding: 14 }}>
      <div style={{ display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap" }}>
        <StatusBadge tone={item.status === "active" ? "success" : "warning"}>{item.status}</StatusBadge>
        {det && !det.enabled && <StatusBadge tone="warning">disabled</StatusBadge>}
        <strong>{item.name}</strong>
        {cfg && <span className="mvx-admin-muted">{cfg.request.method} {host}</span>}
        {det && <StatusBadge tone="neutral">{det.direction}</StatusBadge>}
        <span className="mvx-admin-muted" style={{ fontSize: 12 }}>
          {cfg?.target_type ?? item.target_type}{revisionId ? ` · rev ${revisionId.slice(0, 8)}` : ""}
        </span>
        {sched && sched.kind !== "manual" && (
          <span className="mvx-admin-muted" style={{ fontSize: 12 }}>
            {sched.kind === "interval" ? `every ${sched.interval_seconds}s` : sched.cron_expr} ({sched.timezone})
            {sched.enabled ? "" : " · off"}
          </span>
        )}
        {(item.tags ?? []).map(t => <StatusBadge key={t} tone="neutral">{t}</StatusBadge>)}
        {last && (
          <StatusBadge tone={last.status === "success" ? "success" : last.status === "failed" ? "danger" : "warning"}>
            last: {last.status}
          </StatusBadge>
        )}
        <span style={{ flex: 1 }} />
        <IconButton aria-label={`Run ${item.name} now`} onClick={onRun}><Play size={15} /></IconButton>
        <IconButton aria-label={`Edit ${item.name}`} onClick={onEdit}><Pencil size={15} /></IconButton>
        <IconButton aria-label={`Duplicate ${item.name}`} onClick={onDuplicate}><Copy size={15} /></IconButton>
        <IconButton aria-label={`History for ${item.name}`} onClick={onToggleHistory} aria-expanded={historyOpen}><History size={15} /></IconButton>
        <IconButton aria-label={det?.enabled ? `Disable ${item.name}` : `Enable ${item.name}`}
          onClick={() => onToggleEnabled(!(det?.enabled ?? true))} aria-pressed={det?.enabled ?? true}>
          <Power size={15} />
        </IconButton>
        <IconButton aria-label={`Delete ${item.name}`} onClick={onDelete}><Trash2 size={15} /></IconButton>
      </div>
      {historyOpen && (
        <div style={{ marginTop: 12 }}>
          <ApiRunHistory integrationId={item.id} />
        </div>
      )}
    </div>
  );
}

function hostOf(url: string): string {
  try { return new URL(url).hostname; } catch { return ""; }
}
