import React, { useState } from "react";
import { ApiIntegrationSection } from "./api-integrations/ApiIntegrationSection";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Pencil, Trash2, Plus, FileSpreadsheet, Table2, Database, ClipboardList, Globe } from "lucide-react";
import { api, type FormDef, type DevMetric, type DevDimension, type GridDef, type FormMetricMapping, type FormRecordPosting } from "../../api/client";
import { Field, TextInput, Select, FilterChip, Switch, Button, StatusBadge, SectionHeader, EmptyState, useConfirm } from "../../ui";
import { ExcelImportSection, GoogleSheetsImportSection } from "./ImportWizard";

type IntegrationSource = "csv" | "google_sheets" | "rest_api" | "supabase" | "form_records";

// ── Form Records Integration Section ─────────────────────────────────────────

const STATUS_OPTIONS = ["draft", "submitted", "approved", "rejected"];
const AGG_OPTIONS = ["sum", "replace", "average", "min", "max", "count", "last"];

function FormMappingEditor({
  forms,
  metrics,
  dims,
  grids,
  initial,
  onSave,
  onCancel,
}: {
  forms: FormDef[];
  metrics: DevMetric[];
  dims: DevDimension[];
  grids: GridDef[];
  initial?: Partial<FormMetricMapping>;
  onSave: (m: Omit<FormMetricMapping, "id">) => void;
  onCancel: () => void;
}) {
  const [formId, setFormId] = useState(initial?.form_id ?? "");
  const [gridId, setGridId] = useState(initial?.grid_id ?? "");
  const [name, setName] = useState(initial?.name ?? "");
  const [sourceField, setSourceField] = useState(initial?.source_field ?? "");
  const [targetMetricId, setTargetMetricId] = useState(initial?.target_metric_id ?? "");
  const [aggregation, setAggregation] = useState(initial?.aggregation ?? "sum");
  const [postingStatuses, setPostingStatuses] = useState<string[]>(initial?.posting_statuses ?? ["approved"]);
  const [dimMappings, setDimMappings] = useState<Record<string, string>>(initial?.dimension_mappings ?? {});
  const [livePosting, setLivePosting] = useState(initial?.live_posting ?? true);

  const selectedForm = forms.find(f => f.id === formId);
  const formFields = selectedForm?.fields ?? [];

  // When a grid is selected, limit selectable metrics to those in that grid.
  const selectedGrid = grids.find(g => g.id === gridId);
  const gridMetricIds = selectedGrid?.metric_ids ?? null;
  const inputMetrics = metrics.filter(m =>
    m.is_input && (gridMetricIds === null || gridMetricIds.includes(m.id))
  );

  // Dimensions relevant to the selected grid (optional filter for UX clarity)
  const gridDimIds = selectedGrid?.dimension_ids ?? null;
  const relevantDims = dims.filter(d => gridDimIds === null || gridDimIds.includes(d.id));

  const toggleStatus = (s: string) =>
    setPostingStatuses(prev => prev.includes(s) ? prev.filter(x => x !== s) : [...prev, s]);

  return (
    <div className="mvx-panel" style={{ padding: 20, background: "var(--color-surface-subtle)" }}>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, marginBottom: 12 }}>
        <Field label="Integration name">
          <TextInput value={name} onChange={e => setName(e.target.value)}
            placeholder="e.g. HC Requests → hc_cost" />
        </Field>
        <Field label="Source form">
          <Select value={formId} onChange={e => { setFormId(e.target.value); setSourceField(""); setDimMappings({}); }}>
            <option value="">Select form…</option>
            {forms.map(f => <option key={f.id} value={f.id}>{f.label || f.name}</option>)}
          </Select>
        </Field>
      </div>

      {/* Grid selector — filters available metrics and dimensions */}
      <div style={{ marginBottom: 12 }}>
        <Field
          label="Target grid"
          description={selectedGrid
            ? `Grid has ${selectedGrid.metric_ids.length} metric(s) and ${selectedGrid.dimension_ids.length} dimension(s)`
            : "filters metrics and dimensions to those in this grid"}
        >
          <Select value={gridId} onChange={e => { setGridId(e.target.value); setTargetMetricId(""); }}>
            <option value="">— All metrics (no grid filter) —</option>
            {grids.map(g => <option key={g.id} value={g.id}>{g.name}</option>)}
          </Select>
        </Field>
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, marginBottom: 12 }}>
        <Field label="Source value field">
          <Select value={sourceField} onChange={e => setSourceField(e.target.value)}>
            <option value="">Select field…</option>
            {formFields.filter(f => f.type === "number").map(f => (
              <option key={f.name} value={f.name}>{f.label || f.name}</option>
            ))}
          </Select>
        </Field>
        <Field
          label={selectedGrid ? `Target input metric (from ${selectedGrid.name})` : "Target input metric"}
          error={gridId && inputMetrics.length === 0 ? "No input metrics in selected grid" : undefined}
        >
          <Select value={targetMetricId} onChange={e => setTargetMetricId(e.target.value)}
            error={Boolean(gridId && !targetMetricId)}>
            <option value="">Select metric…</option>
            {inputMetrics.map(m => <option key={m.id} value={m.id}>{m.name}</option>)}
          </Select>
        </Field>
      </div>

      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, marginBottom: 12 }}>
        <Field label="Aggregation">
          <Select value={aggregation} onChange={e => setAggregation(e.target.value)}>
            {AGG_OPTIONS.map(a => <option key={a} value={a}>{a}</option>)}
          </Select>
        </Field>
        <Field label="Post when status is">
          <div style={{ display: "flex", gap: 6, flexWrap: "wrap" }}>
            {STATUS_OPTIONS.map(s => (
              <FilterChip key={s} active={postingStatuses.includes(s)} onClick={() => toggleStatus(s)}>
                {s}
              </FilterChip>
            ))}
          </div>
        </Field>
      </div>

      {relevantDims.length > 0 && formFields.length > 0 && (
        <div style={{ marginBottom: 12 }}>
          <div style={{ fontSize: 13, fontWeight: 500, marginBottom: 6 }}>
            Dimension field mappings
            {selectedGrid && <span className="mvx-admin-muted" style={{ fontWeight: 400, marginLeft: 4 }}>(dimensions in {selectedGrid.name})</span>}
          </div>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 8 }}>
            {relevantDims.map(d => (
              <label key={d.id} style={{ fontSize: 13, display: "flex", alignItems: "center", gap: 8 }}>
                <span style={{ minWidth: 100 }}>{d.name}</span>
                <Select
                  value={dimMappings[d.id] ?? ""}
                  onChange={e => {
                    const v = e.target.value;
                    setDimMappings(prev => v ? { ...prev, [d.id]: v } : Object.fromEntries(Object.entries(prev).filter(([k]) => k !== d.id)));
                  }}
                  style={{ flex: 1 }}
                >
                  <option value="">— skip —</option>
                  {formFields.filter(f => f.type === "dimension" || f.type === "text" || f.type === "select").map(f => (
                    <option key={f.name} value={f.name}>{f.label || f.name}</option>
                  ))}
                </Select>
              </label>
            ))}
          </div>
        </div>
      )}

      <div style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13, marginBottom: 12 }}>
        <Switch checked={livePosting} onChange={setLivePosting} aria-label="Live update" />
        <span style={{ fontWeight: 500 }}>Live update</span>
        <span className="mvx-admin-muted">— automatically sync when record status changes</span>
      </div>

      <div className="mvx-admin-inline-form">
        <Button
          variant="primary"
          disabled={!name || !formId || !sourceField || !targetMetricId}
          onClick={() => {
            if (!name || !formId || !sourceField || !targetMetricId) return;
            onSave({ form_id: formId, grid_id: gridId || undefined, name,
              source_field: sourceField, target_metric_id: targetMetricId,
              aggregation, posting_statuses: postingStatuses, dimension_mappings: dimMappings,
              live_posting: livePosting });
          }}
        >
          Save
        </Button>
        <Button onClick={onCancel}>Cancel</Button>
      </div>
    </div>
  );
}

function FormMappingCard({
  mapping,
  forms,
  metrics,
  dims,
  grids,
  onDelete,
  onSave,
}: {
  mapping: FormMetricMapping;
  forms: FormDef[];
  metrics: DevMetric[];
  dims: DevDimension[];
  grids: GridDef[];
  onDelete: () => void;
  onSave: (m: Omit<FormMetricMapping, "id">) => void;
}) {
  const qc = useQueryClient();
  const [editing, setEditing] = useState(false);
  const [previews, setPreviews] = useState<FormRecordPosting[] | null>(null);
  const [backfillMsg, setBackfillMsg] = useState("");

  const form = forms.find(f => f.id === mapping.form_id);
  const metric = metrics.find(m => m.id === mapping.target_metric_id);
  const grid = grids.find(g => g.id === mapping.grid_id);

  const doBackfill = async () => {
    setBackfillMsg("Running…");
    try {
      const res = await api.backfillFormMapping(mapping.id);
      setBackfillMsg(`Done — ${res.records_processed} record(s) processed`);
      qc.invalidateQueries({ queryKey: ["form-mappings"] });
    } catch {
      setBackfillMsg("Backfill failed");
    }
  };

  const loadPreview = async () => {
    if (previews) { setPreviews(null); return; }
    const p = await api.previewFormMapping(mapping.id);
    setPreviews(p);
  };

  if (editing) {
    return (
      <FormMappingEditor
        forms={forms} metrics={metrics} dims={dims} grids={grids}
        initial={mapping}
        onSave={m => { onSave(m); setEditing(false); }}
        onCancel={() => setEditing(false)}
      />
    );
  }

  return (
    <div className="mvx-panel" style={{ padding: 16 }}>
      <div style={{ display: "flex", justifyContent: "space-between", alignItems: "flex-start", gap: 12 }}>
        <div>
          <div style={{ fontWeight: 600, fontSize: 14, marginBottom: 4, display: "flex", alignItems: "center", gap: 8 }}>
            {mapping.name}
            {grid && <StatusBadge tone="success">Grid: {grid.name}</StatusBadge>}
          </div>
          <div className="mvx-admin-muted">
            <span style={{ marginRight: 12 }}>Form: <b>{form?.label || form?.name || mapping.form_id.slice(0, 8)}</b></span>
            <span style={{ marginRight: 12 }}>Field: <b>{mapping.source_field}</b></span>
            <span style={{ marginRight: 12 }}>→ Metric: <b>{metric?.name || mapping.target_metric_id.slice(0, 8)}</b></span>
            <span style={{ marginRight: 12 }}>→ {grid ? <><b>{grid.name}</b> grid</> : "all grids using this metric"}</span>
            <span>Agg: <b>{mapping.aggregation}</b></span>
          </div>
          <div className="mvx-admin-muted" style={{ marginTop: 4, display: "flex", alignItems: "center", gap: 10, flexWrap: "wrap" }}>
            <span style={{ display: "inline-flex", alignItems: "center", gap: 4 }}>
              Posts when:
              {mapping.posting_statuses.map(s => <StatusBadge key={s} tone="brand">{s}</StatusBadge>)}
            </span>
            <StatusBadge tone={mapping.live_posting ? "success" : "neutral"}>
              {mapping.live_posting ? "live update on" : "live update off"}
            </StatusBadge>
          </div>
        </div>
        <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
          <Button size="sm" onClick={loadPreview}>{previews ? "Hide" : "Preview"}</Button>
          <Button size="sm" onClick={doBackfill}>Backfill</Button>
          <Button size="sm" variant="ghost" leadingIcon={<Pencil size={13} />} onClick={() => setEditing(true)}>Edit</Button>
          <Button size="sm" variant="dangerSecondary" leadingIcon={<Trash2 size={13} />} onClick={onDelete}>Delete</Button>
        </div>
      </div>
      {backfillMsg && <div style={{ fontSize: 12, color: "var(--color-success)", marginTop: 8 }}>{backfillMsg}</div>}
      {previews !== null && (
        <div className="mvx-table-wrap" style={{ marginTop: 12 }}>
          {previews.length === 0 ? (
            <div className="mvx-admin-muted">No postings yet.</div>
          ) : (
            <table className="mvx-table mvx-table--compact">
              <thead>
                <tr>
                  {["Record ID", "Revision", "Dimensions", "Value", "Posted at"].map(h => (
                    <th key={h}>{h}</th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {previews.map((p, i) => (
                  <tr key={i}>
                    <td className="mvx-admin-mono">{p.form_record_id.slice(0, 8)}</td>
                    <td className="mvx-admin-mono">{p.revision_id?.slice(0, 8) ?? "—"}</td>
                    <td className="mvx-admin-mono">
                      {Object.entries(p.dim_members ?? {}).map(([k, v]) => `${k.slice(0, 6)}:${v}`).join(", ") || "—"}
                    </td>
                    <td style={{ fontWeight: 500 }}>{p.posted_value ?? "—"}</td>
                    <td className="mvx-admin-muted">{p.posted_at ? new Date(p.posted_at).toLocaleString() : "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      )}
    </div>
  );
}

function FormRecordsSection() {
  const qc = useQueryClient();
  const [showNew, setShowNew] = useState(false);

  const { data: mappings = [] } = useQuery({ queryKey: ["form-mappings"], queryFn: () => api.listFormMappings() });
  const { data: forms = [] } = useQuery({ queryKey: ["forms"], queryFn: () => api.listForms() });
  const { data: model } = useQuery({ queryKey: ["dev-model"], queryFn: () => api.getDevModel() });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions-all"], queryFn: () => api.getDevDimensions() });
  const { data: gridsRaw = [] } = useQuery({ queryKey: ["dev-grids"], queryFn: () => api.listGrids() });
  // Deduplicate by name — same grid may exist in multiple revisions.
  const grids = gridsRaw.filter((g, i, arr) => arr.findIndex(x => x.name === g.name) === i);

  const metrics: DevMetric[] = model?.metrics ?? [];
  const inv = () => qc.invalidateQueries({ queryKey: ["form-mappings"] });
  const { confirm, confirmElement } = useConfirm();

  const doCreate = async (m: Omit<FormMetricMapping, "id">) => {
    await api.createFormMapping(m);
    setShowNew(false);
    inv();
  };

  const doUpdate = async (id: string, m: Omit<FormMetricMapping, "id">) => {
    await api.updateFormMapping(id, m);
    inv();
  };

  const doDelete = (id: string) => {
    confirm({
      title: "Delete integration?",
      body: "This removes the form-to-metric mapping and stops future postings.",
      confirmLabel: "Delete integration",
      onConfirm: async () => { await api.deleteFormMapping(id); inv(); },
    });
  };

  return (
    <div>
      <SectionHeader
        title="Form Records → Metrics"
        subtitle="Post submitted or approved form records to input metrics with aggregation rules."
        actions={
          <Button variant="primary" leadingIcon={<Plus size={14} />} onClick={() => setShowNew(true)}>
            New integration
          </Button>
        }
      />

      {showNew && (
        <div style={{ marginBottom: 16 }}>
          <FormMappingEditor
            forms={forms} metrics={metrics} dims={dims} grids={grids}
            onSave={doCreate}
            onCancel={() => setShowNew(false)}
          />
        </div>
      )}

      {mappings.length === 0 && !showNew && (
        <EmptyState label="No form integrations yet. Create one to automatically post form record values to input metrics." />
      )}

      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        {mappings.map(m => (
          <FormMappingCard
            key={m.id}
            mapping={m}
            forms={forms}
            metrics={metrics}
            dims={dims}
            grids={grids}
            onDelete={() => doDelete(m.id)}
            onSave={upd => doUpdate(m.id, upd)}
          />
        ))}
      </div>
      {confirmElement}
    </div>
  );
}

// ── Excel/CSV Integrations Tab ─────────────────────────────────────────────────

export function IntegrationsTab({ revisionId }: { revisionId?: string } = {}) {
  const [activeSource, setActiveSource] = useState<IntegrationSource>("csv");

  const sources: { id: IntegrationSource; icon: React.ReactNode; title: string; description: string; available: boolean }[] = [
    {
      id: "csv",
      icon: <FileSpreadsheet size={24} />,
      title: "Excel / CSV Import",
      description: "Upload .xlsx or .csv files, map columns, validate, and commit data.",
      available: true,
    },
    {
      id: "google_sheets",
      icon: <Table2 size={24} />,
      title: "Google Sheets",
      description: "Paste a link-shared sheet URL, map columns, and re-sync on demand.",
      available: true,
    },
    {
      id: "rest_api",
      icon: <Globe size={24} />,
      title: "REST API",
      description: "Connect any JSON API, configure authentication, map data, and run or schedule syncs.",
      available: true,
    },
    {
      id: "supabase",
      icon: <Database size={24} />,
      title: "Supabase",
      description: "Import data directly from a Supabase table or view.",
      available: false,
    },
    {
      id: "form_records",
      icon: <ClipboardList size={24} />,
      title: "Form Records",
      description: "Post submitted or approved form records to input metrics.",
      available: true,
    },
  ];

  return (
    <div>
      {/* Source tiles */}
      <div className="mvx-source-tiles">
        {sources.map(src => (
          <button
            key={src.id}
            type="button"
            disabled={!src.available}
            onClick={() => src.available && setActiveSource(src.id)}
            className={["mvx-source-tile", activeSource === src.id ? "mvx-source-tile--active" : ""].filter(Boolean).join(" ")}
          >
            <div className="mvx-source-tile__icon">{src.icon}</div>
            <div className="mvx-source-tile__title">{src.title}</div>
            <div className="mvx-source-tile__description">{src.description}</div>
            {!src.available && <StatusBadge className="mvx-source-tile__badge">Coming soon</StatusBadge>}
          </button>
        ))}
      </div>

      {/* Source content */}
      {activeSource === "csv" && <ExcelImportSection />}

      {activeSource === "google_sheets" && <GoogleSheetsImportSection />}

      {activeSource === "rest_api" && <ApiIntegrationSection revisionId={revisionId} />}

      {activeSource === "supabase" && (
        <EmptyState label="Supabase integration is coming soon — import data directly from a Supabase table or view." />
      )}

      {activeSource === "form_records" && <FormRecordsSection />}
    </div>
  );
}
