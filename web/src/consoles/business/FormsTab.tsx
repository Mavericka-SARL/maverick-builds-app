import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { invalidateModelData } from "../modelDataQueries";
import { X as XIcon, Pencil as PencilIcon, Plus as PlusIcon, Trash2, Upload, Download } from "lucide-react";
import { api, type DemoContext, type Metric, type DevDimension, type DevDimensionMember, type FormDef, type FormRecord, type FormField, type FormImportRowError, type ApiError } from "../../api/client";
import { Tabs, SectionHeader, Button, InlineAlert, LoadingState, StatusBadge, IconButton, EmptyState, Field, Select, TextInput, NumberInput, useConfirm } from "../../ui";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { downloadBlob, fileToBase64 } from "./blobUtils";

// A form's dimension field only ever lets the user pick a real, concrete
// member (e.g. a specific department) — never an aggregate/rollup node,
// unlike the chart/grid context selectors' HierarchicalMemberSelect usage
// (see HierarchicalMemberSelect's leafOnly prop). DevDimensionMember's
// parent_member_id is an ID reference (developer-console shape), not the
// parent_code the shared tree builder keys on, so each member is adapted
// to carry its resolved parent's code instead.
interface DimFormMember extends DevDimensionMember {
  parent_code?: string;
}

function toDimFormMembers(allMembers: DevDimensionMember[]): DimFormMember[] {
  const byId = new Map(allMembers.map(m => [m.id, m]));
  return allMembers.map(m => ({
    ...m,
    label: m.label || m.code,
    parent_code: m.parent_member_id ? byId.get(m.parent_member_id)?.code : undefined,
  }));
}

// ── Shared form field input renderer ──────────────────────────────────────────

export function FormFieldInput({
  f, value, onChange, dims, metrics,
}: {
  f: FormField;
  value: unknown;
  onChange: (v: unknown) => void;
  dims: DevDimension[];
  metrics: Metric[];
}) {
  if (f.type === "dimension") {
    const dim = dims.find(d => d.id === f.dimension_id) ?? dims.find(d => d.name === f.dimension_id);
    const allMembers = toDimFormMembers(dim?.members ?? []);
    const allowedSet = f.allowed_members && f.allowed_members.length > 0 ? new Set(f.allowed_members) : null;
    return (
      <HierarchicalMemberSelect
        ariaLabel={f.label || dim?.name || "Select…"}
        members={allMembers}
        value={String(value ?? "")}
        onChange={onChange}
        placeholder="Select…"
        leafOnly
        isSelectable={allowedSet ? m => allowedSet.has(m.code) : undefined}
        className="mvx-hier-select__trigger--full-width"
      />
    );
  }
  if (f.type === "metric") {
    if (f.metric_id) {
      // Pre-configured metric — just enter the amount.
      return (
        <NumberInput
          value={String(value ?? "")}
          onChange={e => onChange(e.target.value === "" ? "" : parseFloat(e.target.value))}
          style={{ width: "100%" }}
        />
      );
    }
    // Dynamic metric selection — choose which input metric to write to.
    return (
      <Select value={String(value ?? "")} onChange={e => onChange(e.target.value)} style={{ width: "100%" }}>
        <option value="">Select metric…</option>
        {metrics.filter(m => m.is_input).map(m => <option key={m.id} value={m.name}>{m.label || m.name}</option>)}
      </Select>
    );
  }
  if (f.type === "select") {
    return (
      <Select value={String(value ?? "")} onChange={e => onChange(e.target.value)} style={{ width: "100%" }}>
        <option value="">Select…</option>
        {(f.options ?? []).map(o => <option key={o} value={o}>{o}</option>)}
      </Select>
    );
  }
  if (f.type === "boolean") {
    return (
      <input type="checkbox" className="mvx-checkbox"
        checked={!!value}
        onChange={e => onChange(e.target.checked)}
      />
    );
  }
  return (
    <TextInput
      type={f.type === "number" ? "number" : f.type === "date" ? "date" : "text"}
      value={String(value ?? "")}
      onChange={e => onChange(f.type === "number" ? (e.target.value === "" ? "" : parseFloat(e.target.value)) : e.target.value)}
      style={{ width: "100%" }}
    />
  );
}

// ── Forms Tab (CRUD mode) ──────────────────────────────────────────────────────

export function FormsTab() {
  const qc = useQueryClient();
  const [selectedFormId, setSelectedFormId] = useState<string | null>(null);
  const [showNewRecord, setShowNewRecord] = useState(false);
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  const [syncMsg, setSyncMsg] = useState<string | null>(null);
  const [editingRecord, setEditingRecord] = useState<FormRecord | null>(null);
  const [editDraft, setEditDraft] = useState<Record<string, unknown>>({});
  const [editStatus, setEditStatus] = useState<string>("draft");
  const [importErrors, setImportErrors] = useState<FormImportRowError[] | null>(null);
  const formImportRef = React.useRef<HTMLInputElement>(null);
  const { confirm, confirmElement } = useConfirm();

  const { data: formsRaw = [] } = useQuery({
    queryKey: ["forms"],
    queryFn: () => api.listForms(),
    refetchInterval: 20_000,
  });
  const { data: ctx } = useQuery({ queryKey: ["demo"], queryFn: api.getDemo });
  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dimensions"], queryFn: api.getDimensions });
  const dims = dimsRaw as DevDimension[];
  const revisionId = (ctx as DemoContext | undefined)?.revision_id ?? "";
  const { data: metricsRaw = [] } = useQuery({
    queryKey: ["metrics", revisionId],
    queryFn: () => api.getMetrics(revisionId),
    enabled: !!revisionId,
  });
  const metrics = metricsRaw as Metric[];

  const forms = formsRaw as FormDef[];
  // Derive selectedForm from live query data — always reflects the latest definition
  const selectedForm = forms.find(f => f.id === selectedFormId) ?? null;

  const { data: records = [], isLoading: recLoading } = useQuery({
    queryKey: ["records", selectedFormId],
    queryFn: () => api.listRecords(selectedFormId!),
    enabled: !!selectedFormId,
  });

  const createRecord = useMutation({
    mutationFn: () => api.createRecord(selectedFormId!, draft, (ctx as DemoContext | undefined)?.revision_id),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      invalidateModelData(qc);
      setDraft({});
      setShowNewRecord(false);
    },
  });

  const saveEdit = useMutation({
    mutationFn: () => api.updateRecord(editingRecord!.id, editStatus, editDraft),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      invalidateModelData(qc);
      setEditingRecord(null);
    },
  });

  const deleteRec = useMutation({
    mutationFn: (id: string) => api.deleteRecord(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["records"] }),
  });

  const syncForm = useMutation({
    mutationFn: (formId: string) => api.syncForm(formId),
    onSuccess: (data) => {
      invalidateModelData(qc);
      qc.invalidateQueries({ queryKey: ["records"] });
      if (data.mappings === 0) {
        setSyncMsg("No active integrations configured for this form.");
      } else {
        setSyncMsg(`Synced — ${data.records_processed} record(s) applied across ${data.mappings} integration(s).`);
      }
      setTimeout(() => setSyncMsg(null), 5000);
    },
    onError: (err: Error) => {
      setSyncMsg(`Sync failed: ${err.message}`);
      setTimeout(() => setSyncMsg(null), 6000);
    },
  });

  const exportRecords = useMutation({
    mutationFn: () => api.exportFormRecords(selectedFormId!, "xlsx"),
    onSuccess: ({ blob, filename }) => downloadBlob(blob, filename),
  });

  const importRecords = useMutation({
    mutationFn: async (file: File) => {
      if (file.name.toLowerCase().endsWith(".csv")) {
        return api.importFormRecords(selectedFormId!, await file.text());
      }
      return api.importFormRecordsXlsx(selectedFormId!, await fileToBase64(file));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      invalidateModelData(qc);
      setImportErrors(null);
    },
    onError: (e) => setImportErrors((e as ApiError).body?.errors as FormImportRowError[] | undefined ?? null),
    onMutate: () => setImportErrors(null),
  });

  if (forms.length === 0) {
    return <EmptyState label="No forms defined yet. Ask a Developer to create a form in the Developer Console." />;
  }

  return (
    <div>
      {/* Form selector */}
      <Tabs
        tabs={forms.map(f => ({ id: f.id, label: f.label }))}
        active={selectedFormId ?? ""}
        onChange={(id) => { setSelectedFormId(id); setShowNewRecord(false); setDraft({}); }}
        style={{ marginBottom: 24 }}
      />

      {selectedForm && (
        <div>
          {/* Form header with Sync button */}
          <SectionHeader
            title={selectedForm.label}
            actions={
              <div style={{ display: "flex", gap: 8 }}>
                <Button
                  leadingIcon={<Download size={14} />}
                  loading={exportRecords.isPending}
                  loadingLabel="Exporting…"
                  onClick={() => exportRecords.mutate()}
                >
                  Export
                </Button>
                <Button
                  leadingIcon={<Upload size={14} />}
                  loading={importRecords.isPending}
                  loadingLabel="Importing…"
                  onClick={() => formImportRef.current?.click()}
                >
                  Import
                </Button>
                <input
                  ref={formImportRef}
                  type="file"
                  accept=".csv,.xlsx,text/csv,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
                  style={{ display: "none" }}
                  onChange={(e) => {
                    const file = e.target.files?.[0];
                    e.target.value = "";
                    if (file) importRecords.mutate(file);
                  }}
                />
                <Button
                  loading={syncForm.isPending}
                  loadingLabel="Syncing…"
                  onClick={() => { setSyncMsg(null); syncForm.mutate(selectedFormId!); }}
                >
                  Sync to grid
                </Button>
              </div>
            }
          />
          {syncMsg && (
            <InlineAlert tone={syncMsg.startsWith("Sync failed") ? "danger" : "success"} className="mvx-toolbar--spaced">
              {syncMsg}
            </InlineAlert>
          )}
          {importRecords.isError && (
            <div className="mvx-toolbar--spaced">
              <InlineAlert tone="danger">{(importRecords.error as Error).message}</InlineAlert>
              {importErrors && importErrors.length > 0 && (
                <div className="mvx-table-wrap" style={{ marginTop: 8 }}>
                  <table className="mvx-table mvx-table--compact">
                    <thead><tr><th>Row</th><th>Column</th><th>Problem</th></tr></thead>
                    <tbody>
                      {importErrors.map((e, i) => (
                        <tr key={i}>
                          <td>{e.row}</td>
                          <td className="mvx-admin-mono">{e.column}</td>
                          <td>{e.message}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
              )}
            </div>
          )}

          {/* Records table */}
          {recLoading ? <LoadingState /> : (
            <div className="mvx-table-wrap" style={{ marginBottom: 20 }}>
              <table className="mvx-table mvx-table--compact">
                <thead>
                  <tr>
                    {selectedForm.fields.map((f) => (
                      <th key={f.name}>{f.label}</th>
                    ))}
                    <th>Status</th>
                    <th>Created</th>
                    <th style={{ width: 90 }} />
                  </tr>
                </thead>
                <tbody>
                  {(records as FormRecord[]).map((rec) => {
                    const isEditing = editingRecord?.id === rec.id;
                    return (
                      <tr key={rec.id} style={isEditing ? { background: "var(--color-brand-50)" } : undefined}>
                        {selectedForm.fields.map((f) => (
                          <td key={f.name}>{String(rec.data[f.name] ?? "—")}</td>
                        ))}
                        <td>
                          <StatusBadge tone={rec.status === "approved" ? "success" : rec.status === "rejected" ? "danger" : rec.status === "submitted" ? "warning" : "neutral"}>
                            {rec.status}
                          </StatusBadge>
                        </td>
                        <td className="mvx-admin-muted">
                          {new Date(rec.created_at).toLocaleDateString()}
                        </td>
                        <td style={{ whiteSpace: "nowrap" }}>
                          <div style={{ display: "flex", gap: 2 }}>
                            <IconButton
                              aria-label={isEditing ? "Cancel editing record" : "Edit record"}
                              title={isEditing ? "Cancel" : "Edit"}
                              size={26}
                              onClick={() => {
                                if (isEditing) {
                                  setEditingRecord(null);
                                } else {
                                  setEditingRecord(rec);
                                  setEditDraft({ ...rec.data });
                                  setEditStatus(rec.status);
                                  setShowNewRecord(false);
                                }
                              }}
                            >
                              {isEditing ? <XIcon size={13} /> : <PencilIcon size={13} />}
                            </IconButton>
                            <IconButton aria-label="Delete record" title="Delete" danger size={26}
                              onClick={() => confirm({ title: "Delete record?", body: "This record will be permanently removed.", confirmLabel: "Delete", onConfirm: () => deleteRec.mutate(rec.id) })}>
                              <Trash2 size={13} />
                            </IconButton>
                          </div>
                        </td>
                      </tr>
                    );
                  })}
                  {(records as FormRecord[]).length === 0 && (
                    <tr><td colSpan={selectedForm.fields.length + 3} className="mvx-admin-muted" style={{ textAlign: "center", padding: 24 }}>
                      No records yet.
                    </td></tr>
                  )}
                </tbody>
              </table>
            </div>
          )}
          {confirmElement}

          {/* Edit record form */}
          {editingRecord && selectedForm && (
            <div className="mvx-panel" style={{ marginBottom: 16, maxWidth: 560, padding: 20, borderColor: "var(--color-brand-200)", background: "var(--color-brand-50)" }}>
              <div style={{ fontSize: 13, fontWeight: 600, color: "var(--color-brand-700)", marginBottom: 16 }}>Edit Record</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
                {selectedForm.fields.map((f: FormField) => (
                  <Field key={f.name} label={f.label} required={f.required}>
                    <FormFieldInput f={f} value={editDraft[f.name]} onChange={v => setEditDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics} />
                  </Field>
                ))}
                <Field label="Status">
                  <Select value={editStatus} onChange={(e) => setEditStatus(e.target.value)} style={{ alignSelf: "flex-start" }}>
                    {["draft", "submitted", "approved", "rejected"].map((s) => (
                      <option key={s} value={s}>{s}</option>
                    ))}
                  </Select>
                </Field>
                <div style={{ display: "flex", gap: 8 }}>
                  <Button variant="primary" loading={saveEdit.isPending} loadingLabel="Saving…" onClick={() => saveEdit.mutate()}>
                    Save
                  </Button>
                  <Button onClick={() => setEditingRecord(null)}>Cancel</Button>
                </div>
                {saveEdit.isError && <p className="mvx-admin-error">{(saveEdit.error as Error).message}</p>}
              </div>
            </div>
          )}

          {/* New record form */}
          <Button
            leadingIcon={showNewRecord ? undefined : <PlusIcon size={14} />}
            onClick={() => { setShowNewRecord((v) => !v); setEditingRecord(null); }}
          >
            {showNewRecord ? "Cancel" : "New record"}
          </Button>

          {showNewRecord && (
            <div className="mvx-panel" style={{ marginTop: 16, maxWidth: 560, padding: 20 }}>
              <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
                {selectedForm.fields.map((f: FormField) => (
                  <Field key={f.name} label={f.label} required={f.required}>
                    <FormFieldInput f={f} value={draft[f.name]} onChange={v => setDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics} />
                  </Field>
                ))}
                <Button variant="primary" style={{ alignSelf: "flex-start" }} loading={createRecord.isPending} loadingLabel="Saving…" onClick={() => createRecord.mutate()}>
                  Save
                </Button>
                {createRecord.isError && <p className="mvx-admin-error">{(createRecord.error as Error).message}</p>}
              </div>
            </div>
          )}
        </div>
      )}
    </div>
  );
}
