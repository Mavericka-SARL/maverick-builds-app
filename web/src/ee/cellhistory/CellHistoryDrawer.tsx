import { useQuery } from "@tanstack/react-query";
import { api } from "../../api/client";
import { FeatureGate } from "../../license/FeatureGate";
import { Drawer, InlineAlert, LoadingState, StatusBadge } from "../../ui";

export interface CellRef {
  model_id: string;
  revision_id: string;
  metric_id: string;
  metric_label: string;
  dim_codes: Record<string, string>;
  /** Human labels of the intersection, for the drawer title. */
  member_labels: string[];
}

/**
 * Every value one cell has held: when, who, through what — and, for values
 * later removed, why. Opened from a grid cell's context menu. Enterprise:
 * on other editions the drawer shows which edition unlocks it.
 */
export function CellHistoryDrawer({ cell, onClose }: { cell: CellRef | null; onClose: () => void }) {
  return (
    <Drawer open={cell !== null} onClose={onClose} title={cell ? `History — ${cell.metric_label}${cell.member_labels.length ? " · " + cell.member_labels.join(" / ") : ""}` : "History"} width={520}>
      {cell && (
        <FeatureGate feature="cell_history">
          <HistoryList cell={cell} />
        </FeatureGate>
      )}
    </Drawer>
  );
}

function when(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}

function HistoryList({ cell }: { cell: CellRef }) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["cell-history", cell.revision_id, cell.metric_id, JSON.stringify(cell.dim_codes)],
    queryFn: () => api.getCellHistory(cell),
  });
  if (isLoading) return <LoadingState label="Loading history…" />;
  if (error) return <InlineAlert tone="danger">{(error as Error).message}</InlineAlert>;
  if (!data || data.length === 0) return <p className="mvx-admin-muted" data-testid="cell-history-empty">No value has been entered in this cell yet.</p>;
  return (
    <table className="mvx-table" data-testid="cell-history">
      <thead><tr><th>When</th><th>Value</th><th>By</th><th>How</th></tr></thead>
      <tbody>
        {data.map((e) => (
          <tr key={e.id} data-testid={e.current ? "cell-history-current" : e.deleted_at ? "cell-history-deleted" : "cell-history-row"} style={e.deleted_at ? { opacity: 0.7 } : undefined}>
            <td className="mvx-admin-muted" style={{ whiteSpace: "nowrap" }}>{when(e.entered_at)}</td>
            <td style={{ textAlign: "right", fontVariantNumeric: "tabular-nums", textDecoration: e.deleted_at ? "line-through" : undefined }}>
              {e.value.toLocaleString()} {e.current && <StatusBadge tone="success">current</StatusBadge>}
            </td>
            <td>{e.entered_by.name || e.entered_by.email || "—"}</td>
            <td className="mvx-admin-muted">
              {e.source.kind === "form" ? `form: ${e.source.label}` : e.source.kind === "copied" ? e.source.label : "typed"}
              {e.deleted_at && <div>Removed {when(e.deleted_at)} — {e.delete_reason}</div>}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}
