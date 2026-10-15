import React, { useEffect, useMemo, useState } from "react";
import { useQuery, useMutation, useQueryClient, keepPreviousData } from "@tanstack/react-query";
import { invalidateModelData } from "../modelDataQueries";
import { X as XIcon, Pencil as PencilIcon, Plus as PlusIcon, Trash2, Upload, Download, ArrowUp, ArrowDown, Filter } from "lucide-react";
import { api, statusFieldOf, type Metric, type DevDimension, type DevDimensionMember, type FormDef, type FormRecord, type FormField, recordStatusOptions, type FormImportRowError, type ApiError, type RecordFilter } from "../../api/client";
import { Button, InlineAlert, LoadingState, StatusBadge, IconButton, Field, Select, TextInput, NumberInput, SearchInput, FilterBar, FilterBarGroup, FilterChip, useConfirm } from "../../ui";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { downloadBlob, fileToBase64 } from "./blobUtils";
import { canCreateRecords, canSyncForm, createStatusesOf, createStatusFor, syncFailure, syncRefused, type SyncMessage } from "./formPermissions";
import { describeFilter, displayValue, filterOps, listColumns, listQuery, memberLabels, nextSort, postsMember, statusLabel, type FilterOp, type ListColumn, type ListSort } from "./recordList";

// A record's dimension field only ever lets the user pick a real, concrete
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

// ── Shared record field input renderer ────────────────────────────────────────

// id is the one Field clones onto its child, so the field's label names the
// control it renders. readOnlyMembersOut: the record is the person's own and
// posts this field's member into a metric — within its creator's access —
// so members the person may only read are not offered (the server refuses
// such a save).
export function FormFieldInput({
  f, value, onChange, dims, metrics, id, readOnlyMembersOut,
}: {
  f: FormField;
  value: unknown;
  onChange: (v: unknown) => void;
  dims: DevDimension[];
  metrics: Metric[];
  id?: string;
  readOnlyMembersOut?: boolean;
}) {
  if (f.type === "autocode") {
    // The server gives the code; nobody types it.
    return <TextInput id={id} value={value ? String(value) : "Given when the record is saved"} readOnly disabled style={{ width: "100%" }} />;
  }
  if (f.type === "dimension") {
    const dim = dims.find(d => d.id === f.dimension_id) ?? dims.find(d => d.name === f.dimension_id);
    const allMembers = toDimFormMembers(dim?.members ?? []);
    const allowedSet = f.allowed_members && f.allowed_members.length > 0 ? new Set(f.allowed_members) : null;
    const selectable = allowedSet || readOnlyMembersOut
      ? (m: DimFormMember) => (!allowedSet || allowedSet.has(m.code)) && !(readOnlyMembersOut && m.access === "read")
      : undefined;
    return (
      <HierarchicalMemberSelect
        id={id}
        ariaLabel={f.label || dim?.name || "Select…"}
        members={allMembers}
        value={String(value ?? "")}
        onChange={onChange}
        placeholder="Select…"
        leafOnly
        isSelectable={selectable}
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

/** A record's status, read-only; a record without one says so. */
export function RecordStatusBadge({ status }: { status: string }) {
  return (
    <StatusBadge tone={status === "approved" ? "success" : status === "rejected" ? "danger" : status === "submitted" ? "warning" : "neutral"}>
      {statusLabel(status)}
    </StatusBadge>
  );
}

/** The fields a person fills in: not the status, which is set apart. */
function inputFields(form: FormDef): FormField[] {
  return form.fields.filter(f => f.type !== "status");
}

const PAGE = 50;

// ── Filter builder: one condition at a time, added as a chip ─────────────────

function FilterBuilder({ cols, dims, metrics, statuses, onAdd, onCancel }: {
  cols: ListColumn[];
  dims: DevDimension[];
  metrics: Metric[];
  statuses: string[];
  onAdd: (f: RecordFilter) => void;
  onCancel: () => void;
}) {
  const [key, setKey] = useState(cols[0]?.key ?? "");
  const col = cols.find(c => c.key === key) ?? cols[0];
  const ops = filterOps(col?.kind ?? "text");
  const [op, setOp] = useState<FilterOp>(ops[0].op);
  const [value, setValue] = useState<string>("");
  const opDef = ops.find(o => o.op === op) ?? ops[0];
  const pickKey = (k: string) => {
    setKey(k);
    const c = cols.find(x => x.key === k);
    setOp(filterOps(c?.kind ?? "text")[0].op);
    setValue("");
  };
  const f = col?.field;
  const typed = (): unknown => {
    if (col?.kind === "number") return value === "" ? undefined : Number(value);
    if (col?.kind === "boolean") return value === "true";
    return value;
  };
  const ready = !opDef.takesValue || value !== "" || col?.kind === "status";
  let input: React.ReactNode;
  if (!opDef.takesValue) input = null;
  else if (col?.kind === "status") {
    input = (
      <Select aria-label="Status" value={value} onChange={e => setValue(e.target.value)}>
        <option value="">no status</option>
        {statuses.map(s => <option key={s} value={s}>{s}</option>)}
      </Select>
    );
  } else if (col?.kind === "boolean") {
    input = (
      <Select aria-label="Value" value={value} onChange={e => setValue(e.target.value)}>
        <option value="">Choose…</option>
        <option value="true">Yes</option>
        <option value="false">No</option>
      </Select>
    );
  } else if (f?.type === "dimension" && (op === "eq" || op === "ne")) {
    const labels = memberLabels(dims, f.dimension_id);
    input = (
      <Select aria-label="Member" value={value} onChange={e => setValue(e.target.value)}>
        <option value="">Choose…</option>
        {[...labels].map(([code, label]) => <option key={code} value={code}>{label}</option>)}
      </Select>
    );
  } else if (f?.type === "select" && (op === "eq" || op === "ne")) {
    input = (
      <Select aria-label="Choice" value={value} onChange={e => setValue(e.target.value)}>
        <option value="">Choose…</option>
        {(f.options ?? []).map(o => <option key={o} value={o}>{o}</option>)}
      </Select>
    );
  } else if (f?.type === "metric" && !f.metric_id && (op === "eq" || op === "ne")) {
    input = (
      <Select aria-label="Metric" value={value} onChange={e => setValue(e.target.value)}>
        <option value="">Choose…</option>
        {metrics.filter(m => m.is_input).map(m => <option key={m.id} value={m.name}>{m.label || m.name}</option>)}
      </Select>
    );
  } else {
    input = (
      <TextInput aria-label="Value" value={value} onChange={e => setValue(e.target.value)} style={{ width: 160 }}
        type={col?.kind === "number" ? "number" : col?.kind === "date" || col?.kind === "datetime" ? "date" : "text"} />
    );
  }
  return (
    <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap", padding: "8px 16px" }} role="group" aria-label="New filter">
      <Select aria-label="Field" value={col?.key ?? ""} onChange={e => pickKey(e.target.value)}>
        {cols.map(c => <option key={c.key} value={c.key}>{c.label}</option>)}
      </Select>
      <Select aria-label="Condition" value={op} onChange={e => setOp(e.target.value as FilterOp)}>
        {ops.map(o => <option key={o.op} value={o.op}>{o.label}</option>)}
      </Select>
      {input}
      <Button size="sm" variant="primary" disabled={!ready}
        onClick={() => onAdd(opDef.takesValue ? { field: col.key, op, value: typed() } : { field: col.key, op })}>
        Add filter
      </Button>
      <Button size="sm" variant="ghost" onClick={onCancel}>Cancel</Button>
    </div>
  );
}

// ── Records panel: one table's records, as a dashboard's records widget shows them ──

/**
 * One table's records and everything a person may do with them: search,
 * filter and sort them, add (when the widget offers it), edit, change
 * status, delete, export, import and sync. It is the records widget's body —
 * tables reach business users and business admins only through dashboards,
 * so this is the one place records are worked on. Every action is offered
 * only when the server's per-table and per-record permissions say the
 * caller may take it; "+ New record" also only when the widget's developer
 * added it (newRecordButton).
 */
export function FormPanel({ form, revisionId, newRecordButton = false }: { form: FormDef; revisionId: string; newRecordButton?: boolean }) {
  const qc = useQueryClient();
  const formId = form.id;
  const hasStatus = !!statusFieldOf(form);
  const fields = inputFields(form);
  const cols = useMemo(() => listColumns(form), [form]);
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

  // What the list shows: the search (sent once typing pauses), filters, sort and how many pages.
  const [searchText, setSearchText] = useState("");
  const [search, setSearch] = useState("");
  useEffect(() => {
    const t = setTimeout(() => setSearch(searchText), 250);
    return () => clearTimeout(t);
  }, [searchText]);
  const [filters, setFilters] = useState<RecordFilter[]>([]);
  const [buildingFilter, setBuildingFilter] = useState(false);
  const [sort, setSort] = useState<ListSort>(null);
  // Pages shown of this search, filters and sort: changing any starts again at one.
  const listKey = JSON.stringify([search, filters, sort]);
  const [paging, setPaging] = useState<{ key: string; pages: number }>({ key: listKey, pages: 1 });
  const pages = paging.key === listKey ? paging.pages : 1;
  const query = listQuery({ search, filters, sort, limit: PAGE * pages });

  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dimensions"], queryFn: api.getDimensions });
  // Who is working: a record of their own posts within their access.
  const { data: me } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const dims = dimsRaw as DevDimension[];
  const { data: metricsRaw = [] } = useQuery({
    queryKey: ["metrics", revisionId],
    queryFn: () => api.getMetrics(revisionId),
    enabled: !!revisionId,
  });
  const metrics = metricsRaw as Metric[];
  // What the server lets this caller do with the table's records as a whole.
  const canSync = canSyncForm(form);
  const canCreate = canCreateRecords(form);
  const createStatuses = createStatusesOf(form);
  const newStatus = createStatusFor(form, newStatusPick);

  const { data: page, isLoading: recLoading, isSuccess: recLoaded, isFetching, error: listError } = useQuery({
    queryKey: ["records", formId, query],
    queryFn: () => api.queryRecords(formId, query),
    placeholderData: keepPreviousData,
  });
  const records: FormRecord[] = page?.records ?? [];
  const total = page?.total ?? 0;
  // The record being edited, read from the live list so its status and
  // permissions are the server's current answer.
  const liveEditing = editing ? records.find(r => r.id === editing.id) : undefined;
  const editingRecord = liveEditing?.permissions?.edit ? liveEditing : null;
  // A refresh took the right to edit away (someone decided the record, or
  // removed it): close the editor and say so, rather than let it vanish with
  // the user's changes — and so it cannot come back with a stale draft if a
  // later refresh gives the right back. A record the filters now leave out
  // is not "removed": the editor closes quietly.
  if (editing && recLoaded && !isFetching && !editingRecord) {
    setEditing(null);
    setEditDraft({});
    setEditStatusPick(null);
    if (liveEditing) setEditNotice(`This record can no longer be edited — it is now ${statusLabel(liveEditing.status)}. Your changes were not saved.`);
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
    mutationFn: () => api.createRecord(formId, draft, revisionId || undefined, hasStatus ? newStatus : undefined),
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
      invalidateModelData(qc);
    },
  });
  const shownRecordError = recordError && records.some((r) => r.id === recordError.recordId) ? recordError.message : null;

  const syncForm = useMutation({
    mutationFn: () => api.syncForm(formId),
    onSuccess: (data) => {
      invalidateModelData(qc);
      qc.invalidateQueries({ queryKey: ["records"] });
      if (data.mappings === 0) {
        setSyncMsg({ tone: "success", text: "No active integrations are set up for this table." });
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

  // The export holds the records the list shows, in its order — every
  // page of them.
  const exportRecords = useMutation({
    mutationFn: () => api.exportFormRecords(formId, "xlsx", { ...query, limit: undefined }),
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

  const filtered = search !== "" || filters.length > 0;
  const statusChoices = page?.statuses ?? (hasStatus ? ["draft", "submitted", "approved", "rejected"] : []);

  return (
    <div>
      <div style={{ borderBottom: "1px solid var(--color-border)" }}>
        <div style={{ padding: "12px 16px", background: "var(--color-surface-subtle)", display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, flexWrap: "wrap" }}>
          <span style={{ fontWeight: 600, fontSize: 14 }}>{form.label}</span>
          <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
            {newRecordButton && canCreate && (
              <Button
                size="sm"
                variant={showNewRecord ? "secondary" : "primary"}
                leadingIcon={showNewRecord ? undefined : <PlusIcon size={13} />}
                onClick={() => { setShowNewRecord((v) => !v); closeEditor(); }}
              >
                {showNewRecord ? "Cancel" : "New record"}
              </Button>
            )}
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
            {canCreate && (
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

      {/* New record form: offered when the widget has the button and the server lets this caller create */}
      {newRecordButton && canCreate && showNewRecord && (
        <div className="mvx-panel" style={{ margin: 16, maxWidth: 560, padding: 20 }}>
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            {fields.map((f: FormField) => (
              <Field key={f.name} label={f.label} required={f.required && f.type !== "autocode"}>
                <FormFieldInput f={f} value={draft[f.name]} onChange={v => setDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics}
                  readOnlyMembersOut={postsMember(form, f)} />
              </Field>
            ))}
            {/* The statuses the server accepts for this caller; no choice when there is one. */}
            {hasStatus && createStatuses.length > 1 && (
              <Field label={statusFieldOf(form)?.label || "Status"}>
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

      {/* Search, filters and how many match */}
      <FilterBar>
        <FilterBarGroup>
          <SearchInput value={searchText} onChange={e => setSearchText(e.target.value)} placeholder="Search records…" width={220}
            aria-label={`Search ${form.label}`} />
          <Button size="sm" variant="ghost" leadingIcon={<Filter size={13} />} onClick={() => setBuildingFilter(v => !v)} aria-expanded={buildingFilter}>
            Filter
          </Button>
          {filters.map((f, i) => (
            <FilterChip key={i} active aria-label={`Remove filter: ${describeFilter(f, cols, dims, metrics)}`}
              onClick={() => setFilters(fs => fs.filter((_, j) => j !== i))} onClear={() => setFilters(fs => fs.filter((_, j) => j !== i))}>
              {describeFilter(f, cols, dims, metrics)}
            </FilterChip>
          ))}
        </FilterBarGroup>
        <FilterBarGroup>
          <span className="mvx-admin-muted" style={{ fontSize: 12 }} aria-live="polite">
            {recLoaded ? (filtered ? `${total} matching record${total === 1 ? "" : "s"}` : `${total} record${total === 1 ? "" : "s"}`) : ""}
          </span>
        </FilterBarGroup>
      </FilterBar>
      {buildingFilter && (
        <FilterBuilder cols={cols} dims={dims} metrics={metrics} statuses={statusChoices}
          onAdd={f => { setFilters(fs => [...fs, f]); setBuildingFilter(false); }}
          onCancel={() => setBuildingFilter(false)} />
      )}
      {listError && <InlineAlert tone="danger">Records could not be read — {(listError as Error).message}</InlineAlert>}

      {/* Records table */}
      {recLoading ? <LoadingState /> : (
        <div className="mvx-table-wrap" style={{ overflowY: "auto", maxHeight: 360, marginBottom: 12 }}>
          <table className="mvx-table mvx-table--compact">
            <thead>
              <tr>
                {cols.map((c) => {
                  const sorted = sort?.field === c.key ? (sort.desc ? "descending" : "ascending") : undefined;
                  return (
                    <th key={c.key} aria-sort={sorted ?? "none"}>
                      <button type="button" className="mvx-th-sort" onClick={() => setSort(s => nextSort(s, c.key))}
                        title={`Sort by ${c.label}`}
                        style={{ all: "unset", cursor: "pointer", display: "inline-flex", alignItems: "center", gap: 4 }}>
                        {c.label}
                        {sorted === "ascending" && <ArrowUp size={12} aria-hidden="true" />}
                        {sorted === "descending" && <ArrowDown size={12} aria-hidden="true" />}
                      </button>
                    </th>
                  );
                })}
                <th style={{ width: 70 }} />
              </tr>
            </thead>
            <tbody>
              {records.map((rec) => {
                // The server says what this caller may do with each record;
                // none of it is offered when it says nothing (an older server).
                const statuses = recordStatusOptions(rec);
                const isEditing = editingRecord?.id === rec.id;
                return (
                  <tr key={rec.id} style={isEditing ? { background: "var(--color-brand-50)" } : undefined}>
                    {cols.map((c) => {
                      if (c.kind === "status") {
                        return (
                          <td key={c.key}>
                            {statuses.changeable && !isEditing ? (
                              <Select
                                value={rec.status}
                                onChange={(e) => updateStatus.mutate({ id: rec.id, status: e.target.value })}
                                aria-label={`Record ${c.label.toLowerCase()}`}
                              >
                                {statuses.options.map((s) => (
                                  <option key={s} value={s} disabled={s === ""}>{statusLabel(s)}</option>
                                ))}
                              </Select>
                            ) : (
                              <RecordStatusBadge status={rec.status} />
                            )}
                          </td>
                        );
                      }
                      if (c.key === "created_at") {
                        return <td key={c.key} className="mvx-admin-muted">{new Date(rec.created_at).toLocaleDateString()}</td>;
                      }
                      return <td key={c.key}>{displayValue(c.field, rec.data[c.key], dims, metrics)}</td>;
                    })}
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
              {records.length === 0 && (
                <tr><td colSpan={cols.length + 1} className="mvx-admin-muted" style={{ textAlign: "center", padding: 24 }}>
                  {filtered ? "No records match." : "No records yet."}
                </td></tr>
              )}
            </tbody>
          </table>
          {confirmElement}
        </div>
      )}
      {records.length < total && (
        <div style={{ padding: "0 16px 12px" }}>
          <Button size="sm" variant="ghost" loading={isFetching} loadingLabel="Loading…" onClick={() => setPaging({ key: listKey, pages: pages + 1 })}>
            Show more ({records.length} of {total})
          </Button>
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
                This record was moved to {statusLabel(editingRecord.status)} while you were editing it. Saving keeps that status unless you pick another.
              </InlineAlert>
            )}
            {fields.map((f: FormField) => (
              <Field key={f.name} label={f.label} required={f.required && f.type !== "autocode"}>
                <FormFieldInput f={f} value={editDraft[f.name]} onChange={v => setEditDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics}
                  readOnlyMembersOut={postsMember(form, f) && !!me && editingRecord.created_by === me.user_id} />
              </Field>
            ))}
            {editStatusOptions.options.length > 0 && (
              <Field label={statusFieldOf(form)?.label || "Status"}>
                <Select
                  value={pickedStatus ?? editingRecord.status}
                  onChange={(e) => setEditStatusPick({ from: editingRecord.status, to: e.target.value })}
                  disabled={!editStatusOptions.changeable}
                  style={{ alignSelf: "flex-start" }}
                >
                  {editStatusOptions.options.map((s) => (
                    <option key={s} value={s} disabled={s === ""}>{statusLabel(s)}</option>
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
    </div>
  );
}
