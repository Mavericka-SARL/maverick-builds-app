import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type DevDimension, type DevMetric, type GridDef, type SampleSpec } from "../../api/client";
import { downloadBlob } from "../business/blobUtils";
import { invalidateModelData } from "../modelDataQueries";
import { Button, Checkbox, Dialog, Field, InlineAlert, Tabs, TextInput, useConfirm } from "../../ui";

/**
 * A grid's typed values in bulk (Grids › Values): fill its input metrics
 * with sample values on the server, or clear them — across the grid, or
 * under the members named per dimension. The only bulk clear was a full
 * reload, which empties the whole revision; test data went in by file.
 * Both count first (a dry run), and a fill's rows download as a CSV.
 */
type Mode = "fill" | "clear";

interface MetricDraft { on: boolean; fixed: boolean; value: string; min: string; max: string; decimals: string }

function membersOf(text: Record<string, string>): Record<string, string[]> | undefined {
  const out: Record<string, string[]> = {};
  for (const [dimId, codes] of Object.entries(text)) {
    const list = codes.split(",").map(c => c.trim()).filter(Boolean);
    if (list.length > 0) out[dimId] = list;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

export function GridValuesDialog({ grid, metrics, dims, open, onClose }: {
  grid: GridDef; metrics: DevMetric[]; dims: DevDimension[]; open: boolean; onClose: () => void;
}) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [mode, setMode] = useState<Mode>("fill");
  const inputs = metrics.filter(m => grid.metric_ids.includes(m.id) && m.is_input && m.format !== "text");
  const gridDims = dims.filter(d => grid.dimension_ids.includes(d.id));
  const [drafts, setDrafts] = useState<Record<string, MetricDraft>>(() =>
    Object.fromEntries(inputs.map(m => [m.id, { on: true, fixed: false, value: "0", min: "0", max: "1000", decimals: "0" }])));
  const [memberText, setMemberText] = useState<Record<string, string>>({});
  const [onlyEmpty, setOnlyEmpty] = useState(false);
  const [message, setMessage] = useState<string | null>(null);

  const chosen = inputs.filter(m => drafts[m.id]?.on);
  const specs = (): Record<string, SampleSpec> => Object.fromEntries(chosen.map(m => {
    const d = drafts[m.id];
    return [m.id, d.fixed ? { value: Number(d.value) } : { min: Number(d.min), max: Number(d.max), decimals: Number(d.decimals) }];
  }));
  const sampleBody = (dry: boolean) => ({ metrics: specs(), members: membersOf(memberText), only_empty: onlyEmpty, dry_run: dry });
  const clearBody = (dry: boolean) => ({ metric_ids: chosen.map(m => m.id), members: membersOf(memberText), dry_run: dry });
  const done = () => invalidateModelData(qc);

  const preview = useMutation({ mutationFn: () => api.sampleGridValues(grid.id, sampleBody(true)) });
  const download = useMutation({
    mutationFn: () => api.sampleGridValuesCSV(grid.id, sampleBody(true)),
    onSuccess: ({ blob }) => downloadBlob(blob, `sample values - ${grid.name}.csv`),
  });
  const fill = useMutation({
    mutationFn: () => api.sampleGridValues(grid.id, sampleBody(false)),
    onSuccess: (r) => { setMessage(`Filled ${r.cells} cell${r.cells === 1 ? "" : "s"}.`); done(); },
  });
  const count = useMutation({ mutationFn: () => api.clearGridValues(grid.id, clearBody(true)) });
  const clear = useMutation({
    mutationFn: () => api.clearGridValues(grid.id, clearBody(false)),
    onSuccess: (r) => { setMessage(`Cleared ${r.cells} cell${r.cells === 1 ? "" : "s"}.`); done(); count.reset(); },
  });
  const error = [preview, download, fill, count, clear].find(m => m.isError)?.error as Error | undefined;
  const set = (id: string, patch: Partial<MetricDraft>) => setDrafts(d => ({ ...d, [id]: { ...d[id], ...patch } }));

  return (
    <Dialog open={open} onClose={onClose} title={`Values of ${grid.name}`} width={680}>
      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        <Tabs<Mode> tabs={[{ id: "fill", label: "Fill with sample values" }, { id: "clear", label: "Clear values" }]} active={mode}
          onChange={(m) => { setMode(m); setMessage(null); }} />
        {inputs.length === 0 && <InlineAlert tone="info">This grid has no numeric input metric.</InlineAlert>}
        <table className="mvx-table mvx-table--compact" aria-label="Metrics">
          <tbody>
            {inputs.map(m => {
              const d = drafts[m.id];
              return (
                <tr key={m.id}>
                  <td><Checkbox label={m.label || m.name} checked={d.on} onChange={e => set(m.id, { on: e.target.checked })} /></td>
                  {mode === "fill" && (
                    <td>
                      <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
                        <Checkbox label="Fixed" checked={d.fixed} onChange={e => set(m.id, { fixed: e.target.checked })} />
                        {d.fixed
                          ? <TextInput aria-label={`${m.name} value`} value={d.value} style={{ width: 90 }} onChange={e => set(m.id, { value: e.target.value })} />
                          : <>
                              <TextInput aria-label={`${m.name} from`} value={d.min} style={{ width: 80 }} onChange={e => set(m.id, { min: e.target.value })} />
                              <span>to</span>
                              <TextInput aria-label={`${m.name} to`} value={d.max} style={{ width: 80 }} onChange={e => set(m.id, { max: e.target.value })} />
                              <TextInput aria-label={`${m.name} decimals`} value={d.decimals} style={{ width: 50 }} title="Decimals" onChange={e => set(m.id, { decimals: e.target.value })} />
                            </>}
                      </div>
                    </td>
                  )}
                </tr>
              );
            })}
          </tbody>
        </table>
        {gridDims.map(dim => (
          <Field key={dim.id} label={`${dim.name}: under members (codes, comma-separated; empty for all)`}>
            <TextInput aria-label={`${dim.name} members`} value={memberText[dim.id] ?? ""} placeholder="all"
              onChange={e => setMemberText(t => ({ ...t, [dim.id]: e.target.value }))} />
          </Field>
        ))}
        {mode === "fill" ? (
          <>
            <Checkbox label="Only cells holding no value" checked={onlyEmpty} onChange={e => setOnlyEmpty(e.target.checked)} />
            {preview.data && (
              <p className="mvx-admin-muted" role="status" style={{ margin: 0 }}>
                {preview.data.cells} cell{preview.data.cells === 1 ? "" : "s"} would be filled
                {preview.data.withheld > 0 ? `; ${preview.data.withheld} left that you may not write` : ""}.
              </p>
            )}
            <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
              <Button disabled={chosen.length === 0} loading={preview.isPending} onClick={() => preview.mutate()}>Preview</Button>
              <Button disabled={chosen.length === 0} loading={download.isPending} onClick={() => download.mutate()}>Download CSV</Button>
              <Button variant="primary" disabled={chosen.length === 0} loading={fill.isPending}
                onClick={() => confirm({ title: "Fill with sample values?", body: `Sample values replace the values of ${chosen.length} metric${chosen.length === 1 ? "" : "s"} in "${grid.name}"${onlyEmpty ? " where a cell holds none" : ""}.`, confirmLabel: "Fill", destructive: false, onConfirm: () => fill.mutate() })}>
                Fill
              </Button>
            </div>
          </>
        ) : (
          <>
            {count.data && (
              <p className="mvx-admin-muted" role="status" style={{ margin: 0 }}>
                {count.data.cells} cell{count.data.cells === 1 ? "" : "s"} would be cleared
                {count.data.withheld > 0 ? `; ${count.data.withheld} left that you may not write` : ""}
                {count.data.posted > 0 ? `; ${count.data.posted} left that records post` : ""}.
              </p>
            )}
            <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
              <Button disabled={chosen.length === 0} loading={count.isPending} onClick={() => count.mutate()}>Count</Button>
              <Button variant="danger" disabled={chosen.length === 0} loading={clear.isPending}
                onClick={() => confirm({ title: "Clear values?", body: `The typed values of ${chosen.length} metric${chosen.length === 1 ? "" : "s"} in "${grid.name}"${membersOf(memberText) ? " under the members named" : ""} are deleted. Each cell's history keeps them.`, confirmLabel: "Clear values", onConfirm: () => clear.mutate() })}>
                Clear values
              </Button>
            </div>
          </>
        )}
        {message && <InlineAlert tone="success">{message}</InlineAlert>}
        {error && <p className="mvx-admin-error" role="alert">{error.message}</p>}
      </div>
      {confirmElement}
    </Dialog>
  );
}
