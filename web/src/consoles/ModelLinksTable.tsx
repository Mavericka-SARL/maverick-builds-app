import { useState } from "react";
import type { ModelLink } from "../api/client";
import { DataTable, InlineAlert, StatusBadge, Switch } from "../ui";

export type ModelLinkSide = "target" | "source";

// The model links one side may see, one row per revision copy of a link,
// with the switches that side holds: the source model's developers hold
// "Source side", the tenant's administrators both. A link runs only while
// both are on; the source side's switch is every revision copy's at once.
export function ModelLinksTable({ links, loading, sides, onSwitch, emptyTitle }: {
  links: ModelLink[];
  loading?: boolean;
  sides: ModelLinkSide[];
  onSwitch: (link: ModelLink, side: ModelLinkSide, on: boolean) => Promise<void>;
  emptyTitle: string;
}) {
  const [busy, setBusy] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const flip = async (link: ModelLink, side: ModelLinkSide, on: boolean) => {
    setBusy(link.id + side);
    setError(null);
    try {
      await onSwitch(link, side, on);
    } catch (e) {
      setError((e as Error).message);
    } finally {
      setBusy(null);
    }
  };
  const columns = [
    {
      id: "link", header: "Link",
      cell: (l: ModelLink) => (
        <div style={{ display: "grid", gap: 4 }}>
          <strong>{l.name}</strong>
          <span style={{ display: "flex", gap: 4, flexWrap: "wrap" }}>
            <StatusBadge tone={l.status === "active" ? "success" : "warning"}>{l.status}</StatusBadge>
            {!(l.enabled && l.source_enabled) && <StatusBadge tone="warning">not running</StatusBadge>}
          </span>
        </div>
      ),
    },
    {
      id: "source", header: "Reads",
      cell: (l: ModelLink) => <span>{l.source.application_name} › {l.source.model_name} · grid “{l.source.grid}”</span>,
    },
    {
      id: "target", header: "Into",
      cell: (l: ModelLink) => (
        <span>
          {l.target.application_name} › {l.target.model_name}
          <span className="mvx-admin-muted"> · {l.target.revision}{l.target.active_revision ? " (active)" : ""}</span>
        </span>
      ),
    },
    {
      id: "schedule", header: "Schedule",
      cell: (l: ModelLink) => (
        <span>{l.schedule}{l.owner ? <span className="mvx-admin-muted"> · reads as {l.owner}</span> : null}</span>
      ),
    },
    {
      id: "last", header: "Last run",
      cell: (l: ModelLink) => l.last_run ? (
        <span title={l.last_run.message ?? ""}>
          <StatusBadge tone={l.last_run.status === "success" ? "success" : l.last_run.status === "failed" ? "danger" : "warning"}>
            {l.last_run.status}
          </StatusBadge>
          {l.last_run.finished_at && <span className="mvx-admin-muted"> {new Date(l.last_run.finished_at).toLocaleString()}</span>}
          {l.last_run.error_code && <span className="mvx-admin-muted"> · {l.last_run.error_code}</span>}
        </span>
      ) : <span className="mvx-admin-muted">never run</span>,
    },
    ...sides.map(side => ({
      id: side, header: side === "source" ? "Source side" : "Link's side",
      cell: (l: ModelLink) => {
        const on = side === "source" ? l.source_enabled : l.enabled;
        return (
          <span style={{ display: "grid", gap: 2 }}>
            <Switch checked={on} disabled={busy === l.id + side}
              aria-label={`${side === "source" ? "Source side" : "Link's side"} of ${l.name} (${l.target.revision})`}
              onChange={v => void flip(l, side, v)} />
            {side === "source" && !on && l.source_switched_by && (
              <span className="mvx-admin-muted" style={{ fontSize: 11 }}>off by {l.source_switched_by}</span>
            )}
          </span>
        );
      },
    })),
  ];
  return (
    <div style={{ display: "grid", gap: 8 }}>
      {error && <InlineAlert tone="danger">{error}</InlineAlert>}
      <DataTable columns={columns} rows={links} getRowKey={l => l.id} loading={loading} density="compact"
        emptyTitle={emptyTitle} />
    </div>
  );
}
