import { useEffect, useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { ArrowDown, ArrowUp, Pencil, Plus, Trash2 } from "lucide-react";
import {
  api, type DevDimension, type DevMetric, type ExportPreview, type ExportSpec, type GridDef, type IntegrationDef,
} from "../../api/client";
import {
  Button, Checkbox, Dialog, EmptyState, Field, FilterChip, IconButton, InlineAlert, SectionHeader, SegmentedControl,
  Select, StatusBadge, TagInput, TextInput, useConfirm,
} from "../../ui";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { ExportDownloadButton } from "../ExportDownloadButton";

// Data exports: saved "file_export" integrations — one grid's leaf-level
// values written as CSV, XLSX or JSON in a chosen layout. The same specs the
// AI Developer proposes (create_export_integration) are built and edited
// here, and every download is generated fresh for whoever clicks it.

type Problems = string[];

function summarize(spec: ExportSpec): string {
  const parts: string[] = [(spec.format ?? "csv").toUpperCase()];
  const layout = spec.layout ?? "wide";
  parts.push(layout === "pivot" && spec.pivot_dimension ? `pivot on ${spec.pivot_dimension}` : layout);
  parts.push(spec.metrics?.length ? `${spec.metrics.length} metric(s)` : "all metrics");
  if (spec.filters && Object.keys(spec.filters).length) parts.push(`filtered by ${Object.keys(spec.filters).join(", ")}`);
  return parts.join(" · ");
}

export function ExportSection({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [editing, setEditing] = useState<IntegrationDef | "new" | null>(null);

  const { data: integrations = [] } = useQuery({ queryKey: ["dev-integrations", revisionId], queryFn: () => api.listDevIntegrations(revisionId) });
  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids", revisionId], queryFn: () => api.listGrids(revisionId) });
  const exports = integrations.filter(i => i.type === "file_export");
  const gridName = (id: string) => grids.find(g => g.id === id)?.name ?? "—";

  const remove = (d: IntegrationDef) => confirm({
    title: `Delete export "${d.name}"?`,
    body: "Its download history goes with it, and dashboard buttons that download it are removed.",
    confirmLabel: "Delete export",
    onConfirm: async () => {
      await api.deleteIntegration(d.id);
      qc.invalidateQueries({ queryKey: ["dev-integrations"] });
    },
  });

  return (
    <div>
      <SectionHeader
        title="Data exports"
        subtitle="Download a grid's values as CSV, Excel or JSON in the layout another system expects. Each download is built for whoever clicks it, from what they are allowed to see."
        actions={
          <Button variant="primary" leadingIcon={<Plus size={14} />} onClick={() => setEditing("new")} disabled={grids.length === 0}>
            New export
          </Button>
        }
      />
      {exports.length === 0 && (
        <EmptyState label={grids.length === 0 ? "Create a grid first — an export writes one grid's values." : "No exports yet. Create one to download a grid's data in a fixed format."} />
      )}
      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        {exports.map(d => (
          <div key={d.id} className="mvx-panel" style={{ padding: 16 }} data-testid="export-card">
            <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", gap: 12, flexWrap: "wrap" }}>
              <div style={{ minWidth: 0 }}>
                <div style={{ fontWeight: 600, fontSize: 14, marginBottom: 4, display: "flex", alignItems: "center", gap: 8 }}>
                  {d.name}
                  {d.status === "draft" && <StatusBadge>Draft</StatusBadge>}
                </div>
                <div className="mvx-admin-muted">
                  Grid <b>{gridName(d.target_id)}</b> · {summarize(d.config as ExportSpec)}
                </div>
                {d.tags && d.tags.length > 0 && (
                  <div style={{ display: "flex", gap: 4, marginTop: 6, flexWrap: "wrap" }}>
                    {d.tags.map(t => <FilterChip key={t}>{t}</FilterChip>)}
                  </div>
                )}
              </div>
              <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
                {d.status !== "draft" && <ExportDownloadButton integrationId={d.id} />}
                <IconButton aria-label={`Edit ${d.name}`} onClick={() => setEditing(d)}><Pencil size={14} /></IconButton>
                <IconButton aria-label={`Delete ${d.name}`} onClick={() => remove(d)}><Trash2 size={14} /></IconButton>
              </div>
            </div>
          </div>
        ))}
      </div>
      {editing && (
        <ExportEditor
          revisionId={revisionId}
          grids={grids}
          existing={editing === "new" ? undefined : editing}
          onClose={() => setEditing(null)}
          onSaved={() => { setEditing(null); qc.invalidateQueries({ queryKey: ["dev-integrations"] }); }}
        />
      )}
      {confirmElement}
    </div>
  );
}

// toHierarchy gives the member picker parent codes.
function toHierarchy(d?: DevDimension) {
  if (!d) return [];
  const codeOf = new Map(d.members.map(m => [m.id, m.code]));
  return d.members.map(m => ({ code: m.code, label: m.label || m.code, parent_code: m.parent_member_id ? codeOf.get(m.parent_member_id) : undefined }));
}

function ExportEditor({ revisionId, grids, existing, onClose, onSaved }: {
  revisionId?: string;
  grids: GridDef[];
  existing?: IntegrationDef;
  onClose: () => void;
  onSaved: () => void;
}) {
  const [name, setName] = useState(existing?.name ?? "");
  const [gridId, setGridId] = useState(existing?.target_id ?? grids[0]?.id ?? "");
  const [tags, setTags] = useState<string[]>(existing?.tags ?? []);
  const [spec, setSpec] = useState<ExportSpec>(() => {
    if (!existing) return { format: "csv", layout: "wide" };
    // eslint-disable-next-line @typescript-eslint/no-unused-vars
    const { column_map, sheet_url, import_mode, ...rest } = existing.config;
    return rest as ExportSpec;
  });
  const [preview, setPreview] = useState<ExportPreview | null>(null);
  const [problems, setProblems] = useState<Problems>([]);
  const [previewError, setPreviewError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState<string | null>(null);

  const { data: model } = useQuery({ queryKey: ["dev-model", revisionId], queryFn: () => api.getDevModel(revisionId) });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions", revisionId], queryFn: () => api.getDevDimensions(revisionId) });

  const grid = grids.find(g => g.id === gridId);
  const gridMetrics: DevMetric[] = useMemo(() => {
    const byId = new Map((model?.metrics ?? []).map(m => [m.id, m]));
    return (grid?.metric_ids ?? []).map(id => byId.get(id)).filter((m): m is DevMetric => !!m);
  }, [grid, model]);
  // Server order: a grid's dimensions by name.
  const gridDims: DevDimension[] = useMemo(() => {
    const ids = new Set(grid?.dimension_ids ?? []);
    return dims.filter(d => ids.has(d.id)).sort((a, b) => a.name.localeCompare(b.name));
  }, [grid, dims]);

  const set = (patch: Partial<ExportSpec>) => setSpec(s => ({ ...s, ...patch }));
  const layout = spec.layout ?? "wide";
  const format = spec.format ?? "csv";

  // Row columns, in order: the spec's, else every non-pivot dimension.
  const columnDims = spec.dimensions ?? gridDims.map(d => d.name).filter(n => n !== spec.pivot_dimension);
  const setColumnDims = (names: string[]) => set({ dimensions: names });
  const move = (i: number, by: number) => {
    const next = [...columnDims];
    const [x] = next.splice(i, 1);
    next.splice(i + by, 0, x);
    setColumnDims(next);
  };

  // Live preview, debounced.
  useEffect(() => {
    if (!gridId) return;
    const t = setTimeout(async () => {
      try {
        const p = await api.previewExport({ target_id: gridId, name: name || undefined, config: spec, rows: 20 });
        setPreview(p);
        setProblems([]);
        setPreviewError(null);
      } catch (e) {
        const body = (e as { body?: { problems?: string[] } }).body;
        setPreview(null);
        if (body?.problems?.length) {
          setProblems(body.problems);
          setPreviewError(null);
        } else {
          setProblems([]);
          setPreviewError(e instanceof Error ? e.message : "Preview failed");
        }
      }
    }, 400);
    return () => clearTimeout(t);
  }, [gridId, spec, name]);

  const save = async () => {
    setSaving(true);
    setSaveError(null);
    try {
      if (existing) {
        await api.updateIntegrationConfig(existing.id, spec as Record<string, unknown>);
        await api.updateIntegration(existing.id, { name: name.trim(), target_type: "grid", target_id: gridId, status: "active", tags });
      } else {
        await api.createExportIntegration({ name: name.trim(), target_id: gridId, tags, config: spec }, revisionId);
      }
      onSaved();
    } catch (e) {
      const body = (e as { body?: { problems?: string[] } }).body;
      if (body?.problems?.length) setProblems(body.problems);
      setSaveError(e instanceof Error ? e.message : "Save failed");
    } finally {
      setSaving(false);
    }
  };

  const filters = spec.filters ?? {};
  const setFilter = (dim: string, codes: string[]) => {
    const next = { ...filters };
    if (codes.length) next[dim] = codes; else delete next[dim];
    set({ filters: Object.keys(next).length ? next : undefined });
  };

  return (
    <Dialog open onClose={onClose} title={existing ? `Edit export "${existing.name}"` : "New data export"} width={920}>
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))", gap: 12 }}>
          <Field label="Name" required>
            <TextInput value={name} onChange={e => setName(e.target.value)} placeholder="e.g. Sales to ERP" />
          </Field>
          <Field label="Grid" description={existing ? "An export stays on its grid — create a new one for another grid." : undefined}>
            <Select value={gridId} disabled={!!existing} onChange={e => { setGridId(e.target.value); setSpec({ format, layout: "wide" }); }}>
              {grids.map(g => <option key={g.id} value={g.id}>{g.name}</option>)}
            </Select>
          </Field>
          <Field label="Tags"><TagInput value={tags} onChange={setTags} /></Field>
        </div>

        <div style={{ display: "flex", gap: 16, flexWrap: "wrap", alignItems: "flex-end" }}>
          <Field label="Format">
            <SegmentedControl aria-label="Format" value={format} onChange={v => set({ format: v })}
              segments={[{ id: "csv", label: "CSV" }, { id: "xlsx", label: "Excel" }, { id: "json", label: "JSON" }]} />
          </Field>
          <Field label="Layout">
            <SegmentedControl aria-label="Layout" value={layout}
              onChange={v => set(v === "pivot" ? { layout: v } : { layout: v, pivot_dimension: undefined })}
              segments={[{ id: "wide", label: "Metrics as columns" }, { id: "long", label: "One row per value" }, { id: "pivot", label: "Members as columns" }]} />
          </Field>
          {layout === "pivot" && (
            <Field label="Members across from">
              <Select value={spec.pivot_dimension ?? ""} onChange={e => {
                const pd = e.target.value || undefined;
                set({ pivot_dimension: pd, dimensions: spec.dimensions?.filter(n => n !== pd) });
              }}>
                <option value="">Choose a dimension…</option>
                {gridDims.map(d => <option key={d.id} value={d.name}>{d.name}</option>)}
              </Select>
            </Field>
          )}
        </div>

        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 16 }}>
          <Field label="Metrics">
            <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
              <Checkbox label="All of the grid's metrics (including ones added later)" checked={!spec.metrics}
                onChange={e => set({ metrics: e.target.checked ? undefined : gridMetrics.map(m => m.name) })} />
              {spec.metrics && gridMetrics.map(m => (
                <Checkbox key={m.id} label={m.label || m.name} checked={spec.metrics!.includes(m.name)}
                  onChange={e => set({ metrics: e.target.checked
                    ? gridMetrics.map(x => x.name).filter(n => n === m.name || spec.metrics!.includes(n))
                    : spec.metrics!.filter(n => n !== m.name) })} />
              ))}
            </div>
          </Field>

          <Field label="Dimension columns" description="A dimension that is not a column must be filtered to one leaf member — exports are leaf-level.">
            <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
              {columnDims.map((n, i) => (
                <div key={n} style={{ display: "flex", alignItems: "center", gap: 6 }}>
                  <Checkbox label={n} checked onChange={() => setColumnDims(columnDims.filter(x => x !== n))} />
                  <span style={{ flex: 1 }} />
                  <IconButton aria-label={`Move ${n} earlier`} disabled={i === 0} onClick={() => move(i, -1)}><ArrowUp size={13} /></IconButton>
                  <IconButton aria-label={`Move ${n} later`} disabled={i === columnDims.length - 1} onClick={() => move(i, 1)}><ArrowDown size={13} /></IconButton>
                </div>
              ))}
              {gridDims.filter(d => !columnDims.includes(d.name) && d.name !== spec.pivot_dimension).map(d => (
                <Checkbox key={d.id} label={`${d.name} (not a column)`} checked={false} onChange={() => setColumnDims([...columnDims, d.name])} />
              ))}
            </div>
          </Field>

          <Field label="Filters" description="Pick members to keep; a parent keeps every leaf under it.">
            <div style={{ display: "flex", flexDirection: "column", gap: 8 }}>
              {gridDims.map(d => {
                const codes = filters[d.name] ?? [];
                return (
                  <div key={d.id}>
                    <div className="mvx-admin-muted" style={{ fontSize: 12, marginBottom: 2 }}>{d.name}</div>
                    <div style={{ display: "flex", gap: 4, flexWrap: "wrap", alignItems: "center" }}>
                      {codes.map(c => <FilterChip key={c} onClear={() => setFilter(d.name, codes.filter(x => x !== c))}>{c}</FilterChip>)}
                      <HierarchicalMemberSelect
                        ariaLabel={`Filter ${d.name}`}
                        members={toHierarchy(d)}
                        value=""
                        placeholder={codes.length ? "Add…" : "All members"}
                        onChange={code => code && !codes.includes(code) && setFilter(d.name, [...codes, code])}
                      />
                    </div>
                  </div>
                );
              })}
            </div>
          </Field>
        </div>

        <div style={{ display: "flex", gap: 12, flexWrap: "wrap", alignItems: "flex-end" }}>
          <Field label="Members as">
            <Select value={spec.member_display ?? "code"} onChange={e => set({ member_display: e.target.value as ExportSpec["member_display"] })}>
              <option value="code">Codes</option>
              <option value="label">Labels</option>
              <option value="code_and_label">Code and label columns</option>
            </Select>
          </Field>
          <Field label="Metrics as">
            <Select value={spec.metric_display ?? "name"} onChange={e => set({ metric_display: e.target.value as ExportSpec["metric_display"] })}>
              <option value="name">Names</option>
              <option value="label">Labels</option>
            </Select>
          </Field>
          <Field label="Decimals">
            <Select value={spec.decimals === undefined ? "" : String(spec.decimals)}
              onChange={e => set({ decimals: e.target.value === "" ? undefined : Number(e.target.value) })}>
              <option value="">Full precision</option>
              {[0, 1, 2, 3, 4, 6].map(n => <option key={n} value={n}>{n}</option>)}
            </Select>
          </Field>
          {format === "csv" && (
            <>
              <Field label="Delimiter">
                <Select value={spec.delimiter ?? ","} onChange={e => set({ delimiter: e.target.value as ExportSpec["delimiter"] })}>
                  <option value=",">Comma ,</option>
                  <option value=";">Semicolon ;</option>
                  <option value="tab">Tab</option>
                  <option value="|">Pipe |</option>
                </Select>
              </Field>
              <Field label="Decimal separator">
                <Select value={spec.decimal_separator ?? "."} onChange={e => set({ decimal_separator: e.target.value as ExportSpec["decimal_separator"] })}>
                  <option value=".">Point 1.5</option>
                  <option value=",">Comma 1,5</option>
                </Select>
              </Field>
            </>
          )}
          {format === "xlsx" && (
            <Field label="Sheet name">
              <TextInput value={spec.sheet_name ?? ""} placeholder="Data" onChange={e => set({ sheet_name: e.target.value || undefined })} />
            </Field>
          )}
          <Field label="File name">
            <TextInput value={spec.file_name ?? ""} placeholder={name || "export"} onChange={e => set({ file_name: e.target.value || undefined })} />
          </Field>
        </div>
        <div style={{ display: "flex", gap: 16, flexWrap: "wrap" }}>
          {format !== "json" && (
            <Checkbox label="Header row" checked={spec.include_header !== false}
              onChange={e => set({ include_header: e.target.checked ? undefined : false })} />
          )}
          <Checkbox label="Include combinations without values" checked={!!spec.include_empty_rows}
            onChange={e => set({ include_empty_rows: e.target.checked || undefined })} />
        </div>

        {preview && preview.default_header.length > 0 && (
          <Field label="Column names" description="Rename a column as it should appear in the file; leave blank to keep it.">
            <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(200px, 1fr))", gap: 8 }}>
              {preview.default_header.map(h => (
                <TextInput key={h} aria-label={`Rename ${h}`} placeholder={h} value={spec.column_names?.[h] ?? ""}
                  onChange={e => {
                    const next = { ...(spec.column_names ?? {}) };
                    if (e.target.value) next[h] = e.target.value; else delete next[h];
                    set({ column_names: Object.keys(next).length ? next : undefined });
                  }} />
              ))}
            </div>
          </Field>
        )}

        {problems.length > 0 && (
          <InlineAlert tone="danger">
            <b>This export cannot be produced yet</b>
            <ul style={{ margin: "4px 0 0", paddingLeft: 18 }}>{problems.map(p => <li key={p}>{p}</li>)}</ul>
          </InlineAlert>
        )}
        {previewError && <InlineAlert tone="danger">{previewError}</InlineAlert>}
        {preview && (
          <div>
            <div className="mvx-admin-muted" style={{ fontSize: 12, marginBottom: 6 }}>
              Preview of <b>{preview.file_name}</b> — {preview.total_rows.toLocaleString()} row(s){preview.total_rows > preview.rows.length ? `, first ${preview.rows.length} shown` : ""}, from your own view of the grid
            </div>
            {preview.warnings.map(w => <InlineAlert key={w} tone="warning">{w}</InlineAlert>)}
            <div className="mvx-table-wrap" style={{ maxHeight: 260, overflow: "auto" }}>
              <table className="mvx-table mvx-table--compact" data-testid="export-preview">
                <thead><tr>{preview.header.map((h, i) => <th key={i}>{h}</th>)}</tr></thead>
                <tbody>
                  {preview.rows.length === 0 && (
                    <tr><td colSpan={Math.max(preview.header.length, 1)} className="mvx-admin-muted">No values match yet.</td></tr>
                  )}
                  {preview.rows.map((r, i) => <tr key={i}>{r.map((c, j) => <td key={j}>{c}</td>)}</tr>)}
                </tbody>
              </table>
            </div>
          </div>
        )}

        {saveError && <span className="mvx-admin-error">{saveError}</span>}
        <div className="mvx-admin-inline-form" style={{ justifyContent: "flex-end" }}>
          <Button onClick={onClose} disabled={saving}>Cancel</Button>
          <Button variant="primary" loading={saving} loadingLabel="Saving…" onClick={save}
            disabled={!name.trim() || !gridId || problems.length > 0}>
            {existing ? "Save export" : "Create export"}
          </Button>
        </div>
      </div>
    </Dialog>
  );
}
