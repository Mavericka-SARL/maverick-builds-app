import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { api, type Reconciliation } from "../../api/client";
import { Button, EmptyState, ErrorState, Field, InlineAlert, LoadingState, StatusBadge, Textarea, Toolbar, ToolbarGroup, Txt } from "../../ui";
import { downloadBlob } from "../business/blobUtils";

// Integrations › Workbook reconciliation: the workbook a revision rebuilds, kept with it; a
// map of the workbook's cells in model terms (rules, proposed from the
// workbook and what the model knows); the cases to compare (a case that
// changes inputs is recalculated on the server); and the last run's report.
export function ReconcileTab({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const key = ["reconciliation", revisionId];
  const { data, isLoading, error } = useQuery({
    queryKey: key,
    queryFn: () => api.getReconciliation(revisionId!),
    enabled: !!revisionId,
    refetchInterval: (q) => (q.state.data?.reconciliation.status === "running" ? 2000 : false),
  });
  if (!revisionId) return <EmptyState label="Choose a revision (Developer Models)." />;
  if (isLoading) return <LoadingState />;
  if (error) return <ErrorState message={(error as Error).message} />;
  const rec = data!.reconciliation;
  const refresh = () => qc.invalidateQueries({ queryKey: key });
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 20, maxWidth: 1100 }}>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 13 }}>
        <Txt>Compare the workbook this revision rebuilds with the model, cell by cell and scenario by scenario. The workbook, its map and its cases stay with the revision: a copy of the revision and an exported model keep them.</Txt>
      </p>
      <WorkbookSection rec={rec} revisionId={revisionId} onChange={refresh} />
      {rec.workbook_size > 0 && <MapSection key={`${rec.revision_id}:${rec.uploaded_at ?? ""}`} rec={rec} revisionId={revisionId} recalculation={data!.recalculation} onChange={refresh} />}
      {rec.workbook_size > 0 && <RunSection rec={rec} revisionId={revisionId} onChange={refresh} />}
    </div>
  );
}

function WorkbookSection({ rec, revisionId, onChange }: { rec: Reconciliation; revisionId: string; onChange: () => void }) {
  const upload = useMutation({
    mutationFn: async (file: File) => {
      const buf = new Uint8Array(await file.arrayBuffer());
      let bin = "";
      for (let i = 0; i < buf.length; i += 0x8000) bin += String.fromCharCode(...buf.subarray(i, i + 0x8000));
      return api.putReconciliationWorkbook(revisionId, file.name, btoa(bin));
    },
    onSuccess: onChange,
  });
  return (
    <section aria-label="Workbook">
      <h3 style={{ margin: "0 0 8px", fontSize: 15 }}>Workbook</h3>
      {rec.workbook_size > 0 ? (
        <p style={{ margin: "0 0 8px", fontSize: 13 }}>
          <strong>{rec.file_name}</strong> · {Math.round(rec.workbook_size / 1024)} KB · {(rec.sheets ?? []).length} sheets: {(rec.sheets ?? []).join(", ")}
        </p>
      ) : (
        <p className="mvx-admin-muted" style={{ margin: "0 0 8px", fontSize: 13 }}><Txt>No workbook yet: upload the .xlsx the model rebuilds.</Txt></p>
      )}
      <label style={{ fontSize: 13 }}>
        {rec.workbook_size > 0 ? "Replace the workbook: " : "Upload the workbook: "}
        <input type="file" accept=".xlsx,.xlsm" aria-label="Workbook file" disabled={upload.isPending}
          onChange={e => { const f = e.target.files?.[0]; if (f) upload.mutate(f); e.target.value = ""; }} />
      </label>
      {upload.isError && <div style={{ marginTop: 6 }}><InlineAlert tone="danger">{(upload.error as Error).message}</InlineAlert></div>}
    </section>
  );
}

function MapSection({ rec, revisionId, recalculation, onChange }: { rec: Reconciliation; revisionId: string; recalculation: boolean; onChange: () => void }) {
  const [mapText, setMapText] = useState(() => JSON.stringify(rec.map, null, 1));
  const [casesText, setCasesText] = useState(() => JSON.stringify(rec.cases, null, 1));
  const [notes, setNotes] = useState<string[]>([]);
  const [parseError, setParseError] = useState("");
  const propose = useMutation({
    mutationFn: () => api.proposeReconciliationMap(revisionId),
    onSuccess: (p) => { setMapText(JSON.stringify(p.map, null, 1)); setNotes(p.notes); },
  });
  const save = useMutation({
    mutationFn: () => {
      let m: unknown, c: unknown;
      try { m = JSON.parse(mapText); c = JSON.parse(casesText || "[]"); } catch (e) { setParseError((e as Error).message); throw e; }
      setParseError("");
      return api.putReconciliationMap(revisionId, m, c);
    },
    onSuccess: onChange,
  });
  let rules: Array<Record<string, unknown>> = [];
  try { rules = (JSON.parse(mapText) as { rules?: Array<Record<string, unknown>> }).rules ?? []; } catch { /* shown on save */ }
  return (
    <section aria-label="Map and cases">
      <h3 style={{ margin: "0 0 8px", fontSize: 15 }}>Map and cases</h3>
      <Toolbar>
        <ToolbarGroup>
          <Button size="sm" onClick={() => propose.mutate()} loading={propose.isPending} loadingLabel="Reading the workbook…">Propose from the workbook</Button>
          <Button size="sm" variant="primary" onClick={() => save.mutate()} loading={save.isPending}>Save map and cases</Button>
        </ToolbarGroup>
      </Toolbar>
      {notes.length > 0 && (
        <div style={{ margin: "8px 0" }}>
          <InlineAlert tone="info">
            <strong><Txt>Proposed: check it, then save.</Txt></strong>
            <ul style={{ margin: "4px 0 0", paddingLeft: 18 }}>{notes.map((n, i) => <li key={i}>{n}</li>)}</ul>
          </InlineAlert>
        </div>
      )}
      {rules.length > 0 && (
        <table className="mvx-table" style={{ margin: "8px 0", fontSize: 12 }}>
          <thead><tr><th>Rule</th><th>Sheet</th><th>Rows</th><th>Columns</th><th>Metrics</th><th>Members</th></tr></thead>
          <tbody>
            {rules.map((r, i) => (
              <tr key={i}>
                <td>{String(r.name ?? "")}</td>
                <td>{String(r.sheet ?? "")}</td>
                <td>{describeRows(r)}</td>
                <td>{describeColumns(r)}</td>
                <td>{r.metric ? String(r.metric) : `${Object.keys((r.metrics_by_column as object) ?? {}).length || ((r.metrics_by_row as unknown[]) ?? []).length} metrics`}</td>
                <td>{Object.entries((r.members as Record<string, Record<string, unknown>>) ?? {}).map(([d, b]) => `${d} ← ${describeBinding(b)}`).join("; ")}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
      <div style={{ display: "grid", gridTemplateColumns: "minmax(0, 2fr) minmax(0, 1fr)", gap: 12 }}>
        <Field label="Map (rules)">
          <Textarea aria-label="Map" rows={18} value={mapText} onChange={e => setMapText(e.target.value)} style={{ fontFamily: "monospace", fontSize: 12 }} />
        </Field>
        <Field label="Cases">
          <Textarea aria-label="Cases" rows={18} value={casesText} onChange={e => setCasesText(e.target.value)} style={{ fontFamily: "monospace", fontSize: 12 }} />
        </Field>
      </div>
      <p className="mvx-admin-muted" style={{ fontSize: 12, margin: "6px 0 0" }}>
        {recalculation
          ? <Txt>A case with edits (cell and value, or range and copy_from) is recalculated on the server before it is compared; a case without edits compares the values the workbook was saved with.</Txt>
          : <Txt>This server does not recalculate workbooks (RECONCILE_SOFFICE is not set): cases with edits are skipped; a case without edits compares the values the workbook was saved with.</Txt>}
      </p>
      {(parseError || save.isError || propose.isError) && (
        <div style={{ marginTop: 6 }}><InlineAlert tone="danger">{parseError || ((save.error ?? propose.error) as Error).message}</InlineAlert></div>
      )}
    </section>
  );
}

function RunSection({ rec, revisionId, onChange }: { rec: Reconciliation; revisionId: string; onChange: () => void }) {
  const run = useMutation({ mutationFn: () => api.runReconciliation(revisionId), onSuccess: onChange });
  const csv = useMutation({
    mutationFn: () => api.downloadReconciliationCSV(revisionId),
    onSuccess: ({ blob, filename }) => downloadBlob(blob, filename),
  });
  const r = rec.report;
  const running = rec.status === "running";
  return (
    <section aria-label="Report">
      <h3 style={{ margin: "0 0 8px", fontSize: 15 }}>Report</h3>
      <Toolbar>
        <ToolbarGroup>
          <Button size="sm" variant="primary" onClick={() => run.mutate()} loading={running || run.isPending} loadingLabel="Comparing…" disabled={rec.map.rules.length === 0}>Run</Button>
          {r && r.differences_total > 0 && <Button size="sm" onClick={() => csv.mutate()} loading={csv.isPending}>Differences as CSV</Button>}
        </ToolbarGroup>
      </Toolbar>
      {rec.status === "failed" && <div style={{ marginTop: 6 }}><InlineAlert tone="danger">{rec.error}</InlineAlert></div>}
      {run.isError && <div style={{ marginTop: 6 }}><InlineAlert tone="danger">{(run.error as Error).message}</InlineAlert></div>}
      {r && (
        <div style={{ display: "flex", flexDirection: "column", gap: 10, marginTop: 10 }}>
          <div style={{ fontSize: 14 }}>
            <StatusBadge tone={r.matched === r.compared && r.mapping_problems.length === 0 ? "success" : "warning"}>{`${r.matched.toLocaleString()} / ${r.compared.toLocaleString()} cells match`}</StatusBadge>
            <span className="mvx-admin-muted" style={{ marginLeft: 8, fontSize: 12 }}>{`run ${new Date(r.ran_at).toLocaleString()}`}</span>
          </div>
          <table className="mvx-table" style={{ fontSize: 13 }}>
            <thead><tr><th>Case</th><th>Match</th><th>Differ most</th></tr></thead>
            <tbody>
              {r.cases.map(c => (
                <tr key={c.name}>
                  <td>{c.name}</td>
                  <td>{c.status === "skipped" ? <span className="mvx-admin-muted">{`not compared: ${c.reason ?? ""}`}</span> : `${c.matched.toLocaleString()} / ${c.compared.toLocaleString()}`}</td>
                  <td>{c.by_metric.filter(m => m.compared !== m.matched).slice(0, 5).map(m => `${m.name} (${m.compared - m.matched})`).join(", ")}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {r.mapping_problems.length > 0 && (
            <InlineAlert tone="warning">
              <strong><Txt>The map reads the workbook wrongly here (fix the map, not the model):</Txt></strong>
              <ul style={{ margin: "4px 0 0", paddingLeft: 18 }}>{r.mapping_problems.slice(0, 20).map((p, i) => <li key={i}>{p}</li>)}</ul>
            </InlineAlert>
          )}
          {r.differences.length > 0 && (
            <table className="mvx-table" style={{ fontSize: 12 }}>
              <thead><tr><th>Case</th><th>Cell</th><th>Metric</th><th>Members</th><th>Workbook</th><th>Model</th></tr></thead>
              <tbody>
                {r.differences.slice(0, 200).map((d, i) => (
                  <tr key={i}>
                    <td>{d.case}</td>
                    <td>{`${d.sheet}!${d.cell}`}</td>
                    <td>{d.metric}</td>
                    <td>{Object.entries(d.members).map(([k, v]) => `${k}=${v}`).join(", ")}</td>
                    <td>{d.expected}</td>
                    <td>{d.actual}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {r.differences_total > 200 && <span className="mvx-admin-muted" style={{ fontSize: 12 }}>{`The first 200 of ${r.differences_total.toLocaleString()} differences; the CSV has them all.`}</span>}
        </div>
      )}
    </section>
  );
}

function describeRows(r: Record<string, unknown>): string {
  const rows = r.rows as { from: number; to: number } | undefined;
  const list = r.row_list as number[] | undefined;
  const base = rows ? `${rows.from}–${rows.to}` : list ? list.join(", ") : "";
  const b = r.blocks as { first: number; height: number; count: number } | undefined;
  return b ? `${base} of ${b.count} blocks of ${b.height} from row ${b.first}` : base;
}

function describeColumns(r: Record<string, unknown>): string {
  const c = r.columns as { from: string; to: string } | undefined;
  if (c) return `${c.from}–${c.to}`;
  const list = r.column_list as string[] | undefined;
  if (list) return list.join(", ");
  return Object.keys((r.metrics_by_column as object) ?? {}).sort((x, y) => x.length - y.length || x.localeCompare(y)).join(", ");
}

function describeBinding(b: Record<string, unknown>): string {
  const split = b.split ? ` split "${String(b.split)}" part ${String(b.part ?? 1)}` : "";
  if (b.case) return "the case";
  if (b.total) return "total";
  if (b.code) return `"${String(b.code)}"`;
  if (b.cell) return String(b.cell) + split;
  if (b.row) return `row ${String(b.row)}${split}`;
  if (b.column) return `column ${String(b.column)}${b.block_row !== undefined ? ` (block row ${String(b.block_row)})` : ""}${split}`;
  return "?";
}
