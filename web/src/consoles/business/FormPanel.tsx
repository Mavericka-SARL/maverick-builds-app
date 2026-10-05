import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { invalidateModelData } from "../modelDataQueries";
import { X as XIcon, Pencil as PencilIcon, Plus as PlusIcon, Trash2, Upload, Download } from "lucide-react";
import { api, type Metric, type DevDimension, type DevDimensionMember, type FormDef, type FormRecord, type FormField, recordStatusOptions, type FormImportRowError, type ApiError } from "../../api/client";
import { Button, InlineAlert, LoadingState, StatusBadge, IconButton, Field, Select, TextInput, NumberInput, useConfirm } from "../../ui";
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

// id is the one Field clones onto its child, so the field's label names the
// control it renders.
export function FormFieldInput({
  f, value, onChange, dims, metrics, id,
}: {
  f: FormField;
  value: unknown;
  onChange: (v: unknown) => void;
  dims: DevDimension[];
  metrics: Metric[];
  id?: string;
}) {
  if (f.type === "dimension") {
    const dim = dims.find(d => d.id === f.dimension_id) ?? dims.find(d => d.name === f.dimension_id);
    const allMembers = toDimFormMembers(dim?.members ?? []);
    const allowedSet = f.allowed_members && f.allowed_members.length > 0 ? new Set(f.allowed_members) : null;
    return (
      <HierarchicalMemberSelect
        id={id}
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
          id={id}
          value={String(value ?? "")}
          onChange={e => onChange(e.target.value === "" ? "" : parseFloat(e.target.value))}
          style={{ width: "100%" }}
        />
      );
    }
    // Dynamic metric selection — choose which input metric to write to.
    return (
      <Select id={id} value={String(value ?? "")} onChange={e => onChange(e.target.value)} style={{ width: "100%" }}>
        <option value="">Select metric…</option>
        {metrics.filter(m => m.is_input).map(m => <option key={m.id} value={m.name}>{m.label || m.name}</option>)}
      </Select>
    );
  }
  if (f.type === "select") {
    return (
      <Select id={id} value={String(value ?? "")} onChange={e => onChange(e.target.value)} style={{ width: "100%" }}>
        <option value="">Select…</option>
        {(f.options ?? []).map(o => <option key={o} value={o}>{o}</option>)}
      </Select>
    );
  }
  if (f.type === "boolean") {
    return (
      <input type="checkbox" className="mvx-checkbox" id={id}
        checked={!!value}
        onChange={e => onChange(e.target.checked)}
      />
    );
  }
  return (
    <TextInput
      id={id}
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

// ── Form panel: one form's records, as a dashboard's form widget shows them ──

/**
 * One form's records and everything a person may do with them: add, edit,
 * change status, delete, export, import and sync. It is the form widget's
 * body — forms reach business users and business admins only through
 * dashboards, so this is the one place records are worked on. Every action
 * is offered only when the server's per-form and per-record permissions say
 * the caller may take it.
 */
export function FormPanel({ form, revisionId }: { form: FormDef; revisionId: string }) {
  const qc = useQueryClient();
  const formId = form.id;
  const [showNewRecord, setShowNewRecord] = useState(false);
  const [draft, setDraft] = useState<Record<string, unknown>>({});
  // The status the user picked for the new record; null until they pick.
  const [newStatusPick, setNewStatusPick] = useState<string | null>(null);
  const [syncMsg, setSyncMsg] = useState<SyncMessage | null>(null);
  // The record being edited, with its status when the editor opened.
  const [editing, setEditing] = useState<{ id: string; openedStatus: string } | null>(null);
  const [editDraft, setEditDraft] = useState<Record<string, unknown>>({});
  // The status the user picked, with the status it was picked from; null
  // until they pick one. A pick made from a status the record no longer has
  // is void, so a save never undoes a decision made meanwhile.
  const [editStatusPick, setEditStatusPick] = useState<{ from: string; to: string } | null>(null);
  const [editNotice, setEditNotice] = useState<string | null>(null);
  // The latest failed record action and the record it was on. Starting
  // another action clears it, a later failure replaces it, and it is not
  // shown once that record has left the list.
  const [recordError, setRecordError] = useState<{ recordId: string; message: string } | null>(null);
  const [importErrors, setImportErrors] = useState<FormImportRowError[] | null>(null);
  const importRef = React.useRef<HTMLInputElement>(null);
  const { confirm, confirmElement } = useConfirm();

  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dimensions"], queryFn: api.getDimensions });
  const dims = dimsRaw as DevDimension[];
  const { data: metricsRaw = [] } = useQuery({
    queryKey: ["metrics", revisionId],
    queryFn: () => api.getMetrics(revisionId),
    enabled: !!revisionId,
  });
  const metrics = metricsRaw as Metric[];
  // What the server lets this caller do with the form's records as a whole.
  const canSync = canSyncForm(form);
  const createStatuses = createStatusesOf(form);
  const newStatus = createStatusFor(form, newStatusPick);

  const { data: records = [], isLoading: recLoading, isSuccess: recLoaded } = useQuery({
    queryKey: ["records", formId],
    queryFn: () => api.listRecords(formId),
  });
  // The record being edited, read from the live list so its status and
  // permissions are the server's current answer.
  const liveEditing = editing ? (records as FormRecord[]).find(r => r.id === editing.id) : undefined;
  const editingRecord = liveEditing?.permissions?.edit ? liveEditing : null;
  // A refresh took the right to edit away (someone decided the record, or
  // removed it): close the editor and say so, rather than let it vanish with
  // the user's changes — and so it cannot come back with a stale draft if a
  // later refresh gives the right back.
  if (editing && recLoaded && !editingRecord) {
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
    mutationFn: () => api.createRecord(formId, draft, revisionId || undefined, newStatus),
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

  // A status change from the list sends the status alone: the server keeps
  // the fields as it has them, so it cannot undo an edit made after this
  // list was read. It can post the record into metrics, so the figures
  // beside it refresh.
  const updateStatus = useMutation({
    mutationFn: ({ id, status }: { id: string; status: string }) => api.updateRecord(id, { status }),
    onMutate: () => setRecordError(null),
    onError: (err: Error, { id }) => setRecordError({ recordId: id, message: `Status not changed — ${err.message}` }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      invalidateModelData(qc);
    },
  });

  const deleteRec = useMutation({
    mutationFn: (id: string) => api.deleteRecord(id),
    onMutate: () => setRecordError(null),
    onError: (err: Error, id) => setRecordError({ recordId: id, message: `Record not deleted — ${err.message}` }),
    onSuccess: (_res, id) => {
      // Deleting the record being edited closes its editor; it is not
      // "removed by someone else".
      if (editing?.id === id) closeEditor();
      qc.invalidateQueries({ queryKey: ["records"] });
    },
  });
  const shownRecordError = recordError && (records as FormRecord[]).some((r) => r.id === recordError.recordId) ? recordError.message : null;

  const syncForm = useMutation({
    mutationFn: () => api.syncForm(formId),
    onSuccess: (data) => {
      invalidateModelData(qc);
      qc.invalidateQueries({ queryKey: ["records"] });
      if (data.mappings === 0) {
        setSyncMsg({ tone: "success", text: "No active integrations configured for this form." });
      } else {
        setSyncMsg({ tone: "success", text: `Synced — ${data.records_processed} record(s) across ${data.mappings} integration(s).` });
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

  // The export holds the records this caller can see, as the list does.
  const exportRecords = useMutation({
    mutationFn: () => api.exportFormRecords(formId, "xlsx"),
    onSuccess: ({ blob, filename }) => downloadBlob(blob, filename),
  });

  const importRecords = useMutation({
    mutationFn: async (file: File) => {
      if (file.name.toLowerCase().endsWith(".csv")) {
        return api.importFormRecords(formId, await file.text());
      }
      return api.importFormRecordsXlsx(formId, await fileToBase64(file));
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      invalidateModelData(qc);
      setImportErrors(null);
    },
    onError: (e) => setImportErrors((e as ApiError).body?.errors as FormImportRowError[] | undefined ?? null),
    onMutate: () => setImportErrors(null),
  });

  return (
    <div>
      <div style={{ borderBottom: "1px solid var(--color-border)" }}>
        <div style={{ padding: "12px 16px", background: "var(--color-surface-subtle)", display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap" }}>
          <span style={{ fontWeight: 600, fontSize: 14 }}>{form.label}</span>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            <Button
              size="sm"
              leadingIcon={<Download size={13} />}
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
                  size="sm"
                  leadingIcon={<Upload size={13} />}
                  loading={importRecords.isPending}
                  loadingLabel="Importing…"
                  onClick={() => importRef.current?.click()}
                >
                  Import
                </Button>
                <input
                  ref={importRef}
                  type="file"
                  aria-label={`Import records into ${form.label}`}
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
                size="sm"
                loading={syncForm.isPending}
                loadingLabel="Syncing…"
                onClick={() => { setSyncMsg(null); syncForm.mutate(); }}
              >
                Sync to grid
              </Button>
            )}
          </div>
        </div>
        {syncMsg && (
          <InlineAlert tone={syncMsg.tone}>
            {syncMsg.text}
          </InlineAlert>
        )}
        {exportRecords.isError && (
          <InlineAlert tone="danger">Export failed — {(exportRecords.error as Error).message}</InlineAlert>
        )}
        {importRecords.isError && (
          <div>
            <InlineAlert tone="danger">{(importRecords.error as Error).message}</InlineAlert>
            {importErrors && importErrors.length > 0 && (
              <div className="mvx-table-wrap" style={{ margin: "8px 16px" }}>
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
      </div>

      {/* Records table */}
      {recLoading ? <LoadingState /> : (
        <div className="mvx-table-wrap" style={{ overflowY: "auto", maxHeight: 320, marginBottom: 20 }}>
          <table className="mvx-table mvx-table--compact">
            <thead>
              <tr>
                {form.fields.map((f) => (
                  <th key={f.name}>{f.label}</th>
                ))}
                <th>Status</th>
                <th>Created</th>
                <th style={{ width: 70 }} />
              </tr>
            </thead>
            <tbody>
              {(records as FormRecord[]).map((rec) => {
                // The server says what this caller may do with each record;
                // none of it is offered when it says nothing (an older server).
                const statuses = recordStatusOptions(rec);
                const isEditing = editingRecord?.id === rec.id;
                return (
                  <tr key={rec.id} style={isEditing ? { background: "var(--color-brand-50)" } : undefined}>
                    {form.fields.map((f) => (
                      <td key={f.name}>{String(rec.data[f.name] ?? "—")}</td>
                    ))}
                    <td>
                      {statuses.changeable && !isEditing ? (
                        <Select
                          value={rec.status}
                          onChange={(e) => updateStatus.mutate({ id: rec.id, status: e.target.value })}
                          aria-label="Record status"
                        >
                          {statuses.options.map((s) => (
                            <option key={s} value={s}>{s}</option>
                          ))}
                        </Select>
                      ) : (
                        <RecordStatusBadge status={rec.status} />
                      )}
                    </td>
                    <td className="mvx-admin-muted">
                      {new Date(rec.created_at).toLocaleDateString()}
                    </td>
                    <td style={{ whiteSpace: "nowrap" }}>
                      <div style={{ display: "flex", gap: 2 }}>
                        {rec.permissions?.edit && (
                          <IconButton
                            aria-label={isEditing ? "Cancel editing record" : "Edit record"}
                            title={isEditing ? "Cancel" : "Edit"}
                            size={26}
                            onClick={() => {
                              if (isEditing) {
                                closeEditor();
                              } else {
                                setEditing({ id: rec.id, openedStatus: rec.status });
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
                        {rec.permissions?.delete && (
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
                <tr><td colSpan={form.fields.length + 3} className="mvx-admin-muted" style={{ textAlign: "center", padding: 24 }}>
                  No records yet.
                </td></tr>
              )}
            </tbody>
          </table>
          {confirmElement}
        </div>
      )}
      {shownRecordError && (
        <InlineAlert tone="danger" className="mvx-toolbar--spaced">
          {shownRecordError}
        </InlineAlert>
      )}
      {editNotice && (
        <InlineAlert tone="warning" className="mvx-toolbar--spaced">{editNotice}</InlineAlert>
      )}

      {/* Edit record form */}
      {editingRecord && (
        <div className="mvx-panel" style={{ margin: "0 16px 16px", maxWidth: 560, padding: 20, borderColor: "var(--color-brand-200)", background: "var(--color-brand-50)" }}>
          <div style={{ fontSize: 13, fontWeight: 600, color: "var(--color-brand-700)", marginBottom: 16 }}>Edit Record</div>
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            {statusMovedMeanwhile && (
              <InlineAlert tone="warning">
                This record was moved to {editingRecord.status} while you were editing it. Saving keeps that status unless you pick another.
              </InlineAlert>
            )}
            {form.fields.map((f: FormField) => (
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
        <div style={{ padding: "0 16px 16px" }}>
          <Button
            leadingIcon={showNewRecord ? undefined : <PlusIcon size={14} />}
            onClick={() => { setShowNewRecord((v) => !v); closeEditor(); }}
          >
            {showNewRecord ? "Cancel" : "New record"}
          </Button>

          {showNewRecord && (
            <div className="mvx-panel" style={{ marginTop: 16, maxWidth: 560, padding: 20 }}>
              <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
                {form.fields.map((f: FormField) => (
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
