import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { invalidateModelData } from "../modelDataQueries";
import { X as XIcon, Pencil as PencilIcon, Plus as PlusIcon, Trash2, Upload, Download } from "lucide-react";
import { api, type DemoContext, type Metric, type DevDimension, type DevDimensionMember, type FormDef, type FormRecord, type FormField, recordStatusOptions, type FormImportRowError, type ApiError } from "../../api/client";
import { Tabs, SectionHeader, Button, InlineAlert, LoadingState, StatusBadge, IconButton, EmptyState, Field, Select, TextInput, NumberInput, useConfirm } from "../../ui";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { downloadBlob, fileToBase64 } from "./blobUtils";
import { canSyncForm, createStatusesOf, createStatusFor, syncFailure, syncRefused, type SyncMessage } from "./formPermissions";

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

/** A form record's status, read-only. */
export function RecordStatusBadge({ status }: { status: string }) {
  return (
    <StatusBadge tone={status === "approved" ? "success" : status === "rejected" ? "danger" : status === "submitted" ? "warning" : "neutral"}>
      {status}
    </StatusBadge>
  );
}

// ── Forms Tab (CRUD mode) ──────────────────────────────────────────────────────

export function FormsTab() {
  const qc = useQueryClient();
  const [selectedFormId, setSelectedFormId] = useState<string | null>(null);
  const [showNewRecord, setShowNewRecord] = useState(false);
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  // The status the user picked for the new record; null until they pick.
  const [newStatusPick, setNewStatusPick] = useState<string | null>(null);
  const [syncMsg, setSyncMsg] = useState<SyncMessage | null>(null);
  // The record being edited: its id, its form and its status when the
  // editor opened.
  const [editing, setEditing] = useState<{ id: string; formId: string; openedStatus: string } | null>(null);
  const [editDraft, setEditDraft] = useState<Record<string, unknown>>({});
  // The status the user picked, with the status it was picked from; null
  // until they pick one. A pick made from a status the record no longer has
  // is void, so a save never undoes a decision made meanwhile.
  const [editStatusPick, setEditStatusPick] = useState<{ from: string; to: string } | null>(null);
  const [editNotice, setEditNotice] = useState<string | null>(null);
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
  // What the server lets this caller do with the form's records as a whole.
  const canSync = canSyncForm(selectedForm);
  const createStatuses = createStatusesOf(selectedForm);
  const newStatus = createStatusFor(selectedForm, newStatusPick);

  const { data: records = [], isLoading: recLoading, isSuccess: recLoaded } = useQuery({
    queryKey: ["records", selectedFormId],
    queryFn: () => api.listRecords(selectedFormId!),
    enabled: !!selectedFormId,
  });
  // The record being edited, read from the live list so its status and
  // permissions are the server's current answer.
  const liveEditing = editing ? (records as FormRecord[]).find(r => r.id === editing.id) : undefined;
  const editingRecord = liveEditing?.permissions?.edit ? liveEditing : null;
  // A refresh took the right to edit away (someone decided the record, or
  // removed it): close the editor and say so, rather than let it vanish with
  // the user's changes — and so it cannot come back with a stale draft if a
  // later refresh gives the right back.
  if (editing && editing.formId === selectedFormId && recLoaded && !editingRecord) {
    setEditing(null);
    setEditDraft({});
    setEditStatusPick(null);
    setEditNotice(liveEditing
      ? `This record can no longer be edited — it is now ${liveEditing.status}. Your changes were not saved.`
      : "This record can no longer be edited — it was removed. Your changes were not saved.");
  }
  const editStatusOptions = editingRecord ? recordStatusOptions(editingRecord) : { options: [], changeable: false };
  // The status to show and send: the user's pick while it still applies,
  // otherwise the record's live status (which a save then leaves alone).
  const pickedStatus = editingRecord && editStatusPick && editStatusPick.from === editingRecord.status && editStatusPick.to !== editingRecord.status
    ? editStatusPick.to : undefined;
  const statusMovedMeanwhile = !!editing && !!editingRecord && editingRecord.status !== editing.openedStatus;

  const closeEditor = () => {
    setEditing(null);
    setEditDraft({});
    setEditStatusPick(null);
  };

  const createRecord = useMutation({
    mutationFn: () => api.createRecord(selectedFormId!, draft, (ctx as DemoContext | undefined)?.revision_id, newStatus),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      invalidateModelData(qc);
      setDraft({});
      setNewStatusPick(null);
      setShowNewRecord(false);
    },
  });

  const saveEdit = useMutation({
    // The status goes only when the user picked one: otherwise the server
    // keeps the record's status as it has it.
    mutationFn: () => api.updateRecord(editingRecord!.id, { status: pickedStatus, data: editDraft }),
    onSuccess: () => {
      closeEditor();
      qc.invalidateQueries({ queryKey: ["records"] });
      invalidateModelData(qc);
    },
  });

  const deleteRec = useMutation({
    mutationFn: (id: string) => api.deleteRecord(id),
    onSuccess: (_res, id) => {
      // Deleting the record being edited closes its editor; it is not "removed
      // by someone else".
      if (editing?.id === id) closeEditor();
      qc.invalidateQueries({ queryKey: ["records"] });
    },
  });

  const syncForm = useMutation({
    mutationFn: (formId: string) => api.syncForm(formId),
    onSuccess: (data) => {
      invalidateModelData(qc);
      qc.invalidateQueries({ queryKey: ["records"] });
      if (data.mappings === 0) {
        setSyncMsg({ tone: "success", text: "No active integrations configured for this form." });
      } else {
        setSyncMsg({ tone: "success", text: `Synced — ${data.records_processed} record(s) applied across ${data.mappings} integration(s).` });
      }
      setTimeout(() => setSyncMsg(null), 5000);
    },
    onError: (err: Error) => {
      setSyncMsg(syncFailure(err));
      // The list's permissions said sync; reload them so the button follows the server.
      if (syncRefused(err)) qc.invalidateQueries({ queryKey: ["forms"] });
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
    return <EmptyState label="No forms defined yet. A developer builds them under Build › Forms." />;
  }

  return (
    <div>
      {/* Form selector */}
      <Tabs
        tabs={forms.map(f => ({ id: f.id, label: f.label }))}
        active={selectedFormId ?? ""}
        onChange={(id) => { setSelectedFormId(id); setShowNewRecord(false); setDraft({}); setNewStatusPick(null); closeEditor(); setEditNotice(null); }}
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
                {/* Import creates records: offered only to a caller who may create one. */}
                {createStatuses.length > 0 && (
                  <>
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
                  </>
                )}
                {canSync && (
                  <Button
                    loading={syncForm.isPending}
                    loadingLabel="Syncing…"
                    onClick={() => { setSyncMsg(null); syncForm.mutate(selectedFormId!); }}
                  >
                    Sync to grid
                  </Button>
                )}
              </div>
            }
          />
          {syncMsg && (
            <InlineAlert tone={syncMsg.tone} className="mvx-toolbar--spaced">
              {syncMsg.text}
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
                    // No permissions on the record (an older server) means no actions.
                    const canEdit = !!rec.permissions?.edit;
                    const canDelete = !!rec.permissions?.delete;
                    return (
                      <tr key={rec.id} style={isEditing ? { background: "var(--color-brand-50)" } : undefined}>
                        {selectedForm.fields.map((f) => (
                          <td key={f.name}>{String(rec.data[f.name] ?? "—")}</td>
                        ))}
                        <td>
                          <RecordStatusBadge status={rec.status} />
                        </td>
                        <td className="mvx-admin-muted">
                          {new Date(rec.created_at).toLocaleDateString()}
                        </td>
                        <td style={{ whiteSpace: "nowrap" }}>
                          <div style={{ display: "flex", gap: 2 }}>
                            {canEdit && (
                              <IconButton
                                aria-label={isEditing ? "Cancel editing record" : "Edit record"}
                                title={isEditing ? "Cancel" : "Edit"}
                                size={26}
                                onClick={() => {
                                  if (isEditing) {
                                    closeEditor();
                                  } else {
                                    setEditing({ id: rec.id, formId: rec.form_id, openedStatus: rec.status });
                                    setEditDraft({ ...rec.data });
                                    setEditStatusPick(null);
                                    setEditNotice(null);
                                    saveEdit.reset();
                                    setShowNewRecord(false);
                                  }
                                }}
                              >
                                {isEditing ? <XIcon size={13} /> : <PencilIcon size={13} />}
                              </IconButton>
                            )}
                            {canDelete && (
                              <IconButton aria-label="Delete record" title="Delete" danger size={26}
                                onClick={() => confirm({ title: "Delete record?", body: "This record will be permanently removed.", confirmLabel: "Delete", onConfirm: () => deleteRec.mutate(rec.id) })}>
                                <Trash2 size={13} />
                              </IconButton>
                            )}
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
          {deleteRec.isError && (
            <InlineAlert tone="danger" className="mvx-toolbar--spaced">{(deleteRec.error as Error).message}</InlineAlert>
          )}
          {editNotice && (
            <InlineAlert tone="warning" className="mvx-toolbar--spaced">{editNotice}</InlineAlert>
          )}

          {/* Edit record form */}
          {editingRecord && selectedForm && (
            <div className="mvx-panel" style={{ marginBottom: 16, maxWidth: 560, padding: 20, borderColor: "var(--color-brand-200)", background: "var(--color-brand-50)" }}>
              <div style={{ fontSize: 13, fontWeight: 600, color: "var(--color-brand-700)", marginBottom: 16 }}>Edit Record</div>
              <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
                {statusMovedMeanwhile && (
                  <InlineAlert tone="warning">
                    This record was moved to {editingRecord.status} while you were editing it. Saving keeps that status unless you pick another.
                  </InlineAlert>
                )}
                {selectedForm.fields.map((f: FormField) => (
                  <Field key={f.name} label={f.label} required={f.required}>
                    <FormFieldInput f={f} value={editDraft[f.name]} onChange={v => setEditDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics} />
                  </Field>
                ))}
                {editStatusOptions.options.length > 0 && (
                  <Field label="Status">
                    <Select
                      value={pickedStatus ?? editingRecord.status}
                      onChange={(e) => setEditStatusPick({ from: editingRecord.status, to: e.target.value })}
                      disabled={!editStatusOptions.changeable}
                      style={{ alignSelf: "flex-start" }}
                    >
                      {editStatusOptions.options.map((s) => (
                        <option key={s} value={s}>{s}</option>
                      ))}
                    </Select>
                  </Field>
                )}
                <div style={{ display: "flex", gap: 8 }}>
                  <Button variant="primary" loading={saveEdit.isPending} loadingLabel="Saving…" onClick={() => saveEdit.mutate()}>
                    Save
                  </Button>
                  <Button onClick={closeEditor}>Cancel</Button>
                </div>
                {saveEdit.isError && <p className="mvx-admin-error">{(saveEdit.error as Error).message}</p>}
              </div>
            </div>
          )}

          {/* New record form: offered when the server lets this caller create */}
          {createStatuses.length > 0 && (
            <Button
              leadingIcon={showNewRecord ? undefined : <PlusIcon size={14} />}
              onClick={() => { setShowNewRecord((v) => !v); closeEditor(); }}
            >
              {showNewRecord ? "Cancel" : "New record"}
            </Button>
          )}

          {showNewRecord && createStatuses.length > 0 && (
            <div className="mvx-panel" style={{ marginTop: 16, maxWidth: 560, padding: 20 }}>
              <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
                {selectedForm.fields.map((f: FormField) => (
                  <Field key={f.name} label={f.label} required={f.required}>
                    <FormFieldInput f={f} value={draft[f.name]} onChange={v => setDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics} />
                  </Field>
                ))}
                {/* The statuses the server accepts for this caller; no choice when there is one. */}
                {createStatuses.length > 1 && (
                  <Field label="Status">
                    <Select value={newStatus} onChange={(e) => setNewStatusPick(e.target.value)} style={{ alignSelf: "flex-start" }}>
                      {createStatuses.map((s) => (
                        <option key={s} value={s}>{s}</option>
                      ))}
                    </Select>
                  </Field>
                )}
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
