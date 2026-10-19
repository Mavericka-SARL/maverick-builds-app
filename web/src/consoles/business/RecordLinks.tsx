import React, { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Trash2, History as HistoryIcon, Pencil, Plus as PlusIcon } from "lucide-react";
import {
  api, recordStatusOptions, statusFieldOf,
  type DevDimension, type FormDef, type FormField, type FormRecord, type Metric, type RecordDeletePlan,
} from "../../api/client";
import { Button, Dialog, Field, IconButton, InlineAlert, LoadingState, Select, useConfirm } from "../../ui";
import { invalidateModelData } from "../modelDataQueries";
import { FormFieldInput, RecordStatusBadge } from "./RecordFieldInput";
import { canCreateRecords, createStatusesOf, createStatusFor } from "./formPermissions";
import {
  defaultsOf, displayValue, editableFields, isDueToday, isOverdue, keeperOf, memberLabels, pairedField, postsMember,
  localDateTime, recordTitle, singular, statusLabel,
} from "./recordList";

/** How one field of a record reads: a link opens in a new tab; a deadline that has passed says overdue. */
export function RecordFieldValue({ f, data, dims, metrics, onOpenMember }: {
  /** The record's table (kept for callers; the field says how it reads). */
  form?: FormDef;
  f: FormField;
  data: Record<string, unknown>;
  dims: DevDimension[];
  metrics: Metric[];
  /** Set where a member a table keeps may be opened as its record (a deal's account). */
  onOpenMember?: (f: FormField, code: string) => void;
}) {
  const value = data[f.name];
  if (onOpenMember && f.type === "dimension" && typeof value === "string" && value !== "") {
    return (
      <button type="button" className="mvx-link-button" onClick={e => { e.stopPropagation(); onOpenMember(f, value); }}>
        {displayValue(f, value, dims, metrics)}
      </button>
    );
  }
  if (f.type === "url" && typeof value === "string" && value !== "") {
    return <a href={value} target="_blank" rel="noopener noreferrer" onClick={e => e.stopPropagation()}>{value.replace(/^https?:\/\//, "")}</a>;
  }
  const text = displayValue(f, value, dims, metrics);
  if (isOverdue(f, data)) {
    return <span className="mvx-overdue" title="Overdue">{text} · overdue</span>;
  }
  if (isDueToday(f, data)) {
    return <span className="mvx-due-today" title="Due today">{text} · today</span>;
  }
  return <>{text}</>;
}

// ── Fields of a record, with "+ New" on a field naming a member a table keeps ──

/**
 * The fields a person fills in for a record of form: each with its label,
 * marked required — or required together with its pair — and, on a field
 * naming a member a table keeps, "+ New" opening that table's form in place
 * (an account typed in the deal's form becomes a record of Accounts, and is
 * picked).
 */
export function RecordFields({ form, forms, draft, setDraft, dims, metrics, revisionId, readOnlyOut }: {
  form: FormDef;
  forms: FormDef[];
  draft: Record<string, unknown>;
  setDraft: (fn: (d: Record<string, unknown>) => Record<string, unknown>) => void;
  dims: DevDimension[];
  metrics: Metric[];
  revisionId: string;
  readOnlyOut?: (f: FormField) => boolean;
}) {
  const [creating, setCreating] = useState<string | null>(null);
  return (
    <>
      {editableFields(form).map((f) => {
        const keeper = keeperOf(forms, f);
        const canAdd = !!keeper && keeper.id !== form.id && canCreateRecords(keeper);
        const pair = pairedField(form, f);
        const pairFilled = !!pair && draft[pair.name] !== undefined && draft[pair.name] !== "" && draft[pair.name] !== null;
        return (
          <React.Fragment key={f.name}>
            <Field label={f.label || f.name} required={(f.required || pairFilled) && f.type !== "autocode"}
              description={pair ? `Filled in together with ${pair.label || pair.name}` : undefined}>
              <FormFieldInput f={f} value={draft[f.name]} onChange={v => setDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics}
                readOnlyMembersOut={readOnlyOut?.(f)}
                onCreateLinked={canAdd ? () => setCreating(c => (c === f.name ? null : f.name)) : undefined}
                linkedLabel={keeper ? singular(keeper.label) : undefined} />
            </Field>
            {creating === f.name && keeper && (
              <LinkedCreatePanel keeper={keeper} origin={form} originDraft={draft} revisionId={revisionId} dims={dims} metrics={metrics}
                onCancel={() => setCreating(null)}
                onCreated={(code) => { setDraft(d => ({ ...d, [f.name]: code })); setCreating(null); }} />
            )}
          </React.Fragment>
        );
      })}
    </>
  );
}


/**
 * A new record of keeper, a table keeping a dimension, made from another
 * record's form: its fields that name the same dimension as one the origin
 * form has filled start with that value (a new contact made from a deal is
 * the deal's account's). Saved, it is a member, and onCreated gets its code.
 */
function LinkedCreatePanel({ keeper, origin, originDraft, revisionId, dims, metrics, onCreated, onCancel }: {
  keeper: FormDef;
  origin: FormDef;
  originDraft: Record<string, unknown>;
  revisionId: string;
  dims: DevDimension[];
  metrics: Metric[];
  onCreated: (code: string) => void;
  onCancel: () => void;
}) {
  const qc = useQueryClient();
  const [draft, setDraft] = useState<Record<string, unknown>>(() => {
    const start = defaultsOf(keeper);
    for (const f of keeper.fields) {
      if (f.type !== "dimension") continue;
      const same = origin.fields.find(o => o.type === "dimension" && o.dimension_id === f.dimension_id);
      if (same && originDraft[same.name]) start[f.name] = originDraft[same.name];
    }
    return start;
  });
  const save = useMutation({
    mutationFn: () => api.createRecord(keeper.id, draft, revisionId || undefined, statusFieldOf(keeper) ? createStatusFor(keeper, null) : undefined),
    onSuccess: async (rec) => {
      await qc.invalidateQueries({ queryKey: ["dimensions"] });
      qc.invalidateQueries({ queryKey: ["records"] });
      const code = keeper.fields.find(f => f.type === "autocode");
      onCreated(code ? String(rec.data[code.name] ?? "") : "");
    },
  });
  return (
    <div className="mvx-panel" role="group" aria-label={`New ${singular(keeper.label)}`}
      style={{ padding: 14, borderStyle: "dashed", display: "flex", flexDirection: "column", gap: 10 }}>
      <div style={{ fontSize: 13, fontWeight: 600 }}>New {singular(keeper.label).toLowerCase()}</div>
      {editableFields(keeper).filter(f => f.type !== "autocode").map(f => (
        <Field key={f.name} label={f.label || f.name} required={f.required}>
          <FormFieldInput f={f} value={draft[f.name]} onChange={v => setDraft(d => ({ ...d, [f.name]: v }))} dims={dims} metrics={metrics} />
        </Field>
      ))}
      <div style={{ display: "flex", gap: 8 }}>
        <Button size="sm" variant="primary" loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>
          Add {singular(keeper.label).toLowerCase()}
        </Button>
        <Button size="sm" onClick={onCancel}>Cancel</Button>
      </div>
      {save.isError && <p className="mvx-admin-error">{(save.error as Error).message}</p>}
    </div>
  );
}

// ── Related records: the records of other tables naming this one ──

/** The columns a related table's records show: its first fields, but not the one naming this record. */
function relatedColumns(table: FormDef, linkField: string): FormField[] {
  return editableFields(table)
    .filter(f => f.name !== linkField && f.type !== "autocode")
    .slice(0, 5);
}

/**
 * The records of other tables that name record — an account's contacts and
 * deals, a contact's deals at whichever account — each table with how many
 * there are and "Add". A row opens its record (onOpen).
 */
export function RelatedRecordsSection({ form, record, forms, dims, metrics, onOpen, onAdd }: {
  form: FormDef;
  record: FormRecord;
  forms: FormDef[];
  dims: DevDimension[];
  metrics: Metric[];
  onOpen: (table: FormDef, record: FormRecord) => void;
  onAdd: (table: FormDef, field: string, code: string) => void;
}) {
  const { data, isLoading, error } = useQuery({
    queryKey: ["records", "related", record.id],
    queryFn: () => api.recordRelated(record.id),
    enabled: !!form.keeps_dimension_id,
  });
  if (!form.keeps_dimension_id) return null;
  const code = form.fields.find(f => f.type === "autocode");
  const memberCode = code ? String(record.data[code.name] ?? "") : "";
  const self = singular(form.label).toLowerCase();
  return (
    <section aria-label="Related records" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      {isLoading && <LoadingState />}
      {error && <InlineAlert tone="danger">Related records could not be read — {(error as Error).message}</InlineAlert>}
      {data?.related.length === 0 && <span className="mvx-admin-muted" style={{ fontSize: 12 }}>No other table names it.</span>}
      {data?.related.map(rel => {
        const table = forms.find(f => f.id === rel.table.id);
        if (!table) return null;
        const shown = relatedColumns(table, rel.field);
        // "Deals · as Primary Contact" when the link is not simply the record's own kind.
        const how = rel.field_label.toLowerCase() === self ? "" : ` · as ${rel.field_label}`;
        return (
          <div key={rel.table.id + rel.field} role="group" aria-label={`${rel.table.label}${how}`}>
            <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", gap: 8, marginBottom: 4 }}>
              <span style={{ fontSize: 13, fontWeight: 600 }}>
                {rel.table.label}<span className="mvx-admin-muted" style={{ fontWeight: 400 }}>{how}</span>{" "}
                <span className="mvx-board__column-count">{rel.total}</span>
              </span>
              {rel.can_create && (
                <Button size="sm" variant="ghost" leadingIcon={<PlusIcon size={12} />} onClick={() => onAdd(table, rel.field, memberCode)}>
                  Add
                </Button>
              )}
            </div>
            {rel.records.length > 0 ? (
              <div className="mvx-table-wrap">
                <table className="mvx-table mvx-table--compact">
                  <thead><tr>{shown.map(f => <th key={f.name}>{f.label || f.name}</th>)}</tr></thead>
                  <tbody>
                    {rel.records.map(r => (
                      <tr key={r.id} className="mvx-row-link" onClick={() => onOpen(table, r)}
                        tabIndex={0} onKeyDown={e => { if (e.key === "Enter") onOpen(table, r); }}
                        title={`Open ${recordTitle(table, r)}`}>
                        {shown.map(f => (
                          <td key={f.name} style={f.type === "longtext" ? { maxWidth: 280, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" } : undefined}>
                            <RecordFieldValue form={table} f={f} data={r.data} dims={dims} metrics={metrics} />
                          </td>
                        ))}
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            ) : <span className="mvx-admin-muted" style={{ fontSize: 12 }}>None.</span>}
            {rel.total > rel.records.length && (
              <span className="mvx-admin-muted" style={{ fontSize: 12 }}>Showing {rel.records.length} of {rel.total}.</span>
            )}
          </div>
        );
      })}
    </section>
  );
}

// ── History of a record's history fields ──

/** Each value a record's history fields took — a deal's stages — oldest first. */
export function RecordHistorySection({ form, recordId, dims }: { form: FormDef; recordId: string; dims: DevDimension[] }) {
  const tracked = form.fields.some(f => f.history);
  const { data, isLoading } = useQuery({
    queryKey: ["records", "history", recordId],
    queryFn: () => api.recordHistory(recordId),
    enabled: tracked,
  });
  if (!tracked) return null;
  const show = (fieldName: string, v: unknown): string => {
    if (v === null || v === undefined || v === "") return "—";
    if (typeof v === "object" && v && (v as { hidden?: boolean }).hidden) return "(hidden)";
    const f = form.fields.find(x => x.name === fieldName);
    if (f?.type === "dimension") return memberLabels(dims, f.dimension_id).get(String(v)) ?? String(v);
    return String(v);
  };
  return (
    <section aria-label="History" style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      <div style={{ fontSize: 13, fontWeight: 600, display: "flex", alignItems: "center", gap: 6 }}>
        <HistoryIcon size={14} aria-hidden="true" /> History
      </div>
      {isLoading && <LoadingState />}
      {data && data.history.length === 0 && <span className="mvx-admin-muted" style={{ fontSize: 12 }}>No changes yet.</span>}
      {data && data.history.length > 0 && (
        <ol style={{ margin: 0, paddingLeft: 18, fontSize: 12, display: "flex", flexDirection: "column", gap: 3 }}>
          {data.history.map((h, i) => (
            <li key={i}>
              <span className="mvx-admin-muted">{localDateTime(h.changed_at)}</span>{" "}
              {h.field_label}: {h.old === null ? <strong>{show(h.field, h.new)}</strong> : <>{show(h.field, h.old)} → <strong>{show(h.field, h.new)}</strong></>}
              {h.changed_by_name && <span className="mvx-admin-muted"> · {h.changed_by_name}</span>}
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}

// ── Delete, after a confirmation saying what goes with it ──

function planBody(plan: RecordDeletePlan): React.ReactNode {
  if (!plan.can_delete) return plan.refusal ?? "This record cannot be deleted.";
  const parts: React.ReactNode[] = ["This record will be permanently removed."];
  if (plan.cascade.length > 0) {
    parts.push(<> Deleted with it: {plan.cascade.map(c => `${c.table_label} (${c.count})`).join(", ")}.</>);
  }
  if (plan.clear.length > 0) {
    parts.push(<> Emptied: {plan.clear.map(c => `${c.field_label} of ${c.table_label} (${c.count})`).join(", ")}.</>);
  }
  return <>{parts}</>;
}

/**
 * The delete button of a record: on confirmation it deletes it. A record of
 * a table keeping a dimension (withPlan) may take other records with it or
 * be refused: the button first reads what the delete would do and says so.
 */
export function DeleteRecordButton({ record, withPlan = false, onDeleted, onError, variant = "icon" }: {
  record: FormRecord;
  withPlan?: boolean;
  onDeleted?: () => void;
  onError?: (message: string) => void;
  variant?: "icon" | "button";
}) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [refusal, setRefusal] = useState<string | null>(null);
  const del = useMutation({
    mutationFn: () => api.deleteRecord(record.id),
    onError: (err: Error) => onError?.(`Record not deleted — ${err.message}`),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      qc.invalidateQueries({ queryKey: ["dimensions"] });
      invalidateModelData(qc);
      onDeleted?.();
    },
  });
  const ask = async () => {
    setRefusal(null);
    if (!withPlan) {
      confirm({ title: "Delete record?", body: "This record will be permanently removed.", confirmLabel: "Delete", onConfirm: () => del.mutate() });
      return;
    }
    let plan: RecordDeletePlan;
    try {
      plan = await api.recordDeletePlan(record.id);
    } catch (e) {
      onError?.(`Record not deleted — ${(e as Error).message}`);
      return;
    }
    if (!plan.can_delete) {
      if (onError) onError(plan.refusal ?? "This record cannot be deleted.");
      else setRefusal(plan.refusal ?? "This record cannot be deleted.");
      return;
    }
    confirm({ title: "Delete record?", body: planBody(plan), confirmLabel: "Delete", destructive: true, onConfirm: () => del.mutate() });
  };
  return (
    <>
      {variant === "icon" ? (
        <IconButton aria-label="Delete record" title="Delete" danger size={26} onClick={ask}>
          <Trash2 size={13} />
        </IconButton>
      ) : (
        <Button variant="danger" leadingIcon={<Trash2 size={13} />} loading={del.isPending} loadingLabel="Deleting…" onClick={ask}>
          Delete
        </Button>
      )}
      {refusal && <InlineAlert tone="danger">{refusal}</InlineAlert>}
      {confirmElement}
    </>
  );
}

// ── One record in a dialog: what it holds, what links to it, its history ──

/** A record the dialog shows: an existing one, or a new one of form starting from initialData. */
interface RecordStep {
  form: FormDef;
  record: FormRecord | null;
  initialData?: Record<string, unknown>;
}

/**
 * A record in a dialog, as its own page: its fields on the left — to read,
 * or to change after Edit when the server lets the caller — and on the right
 * the records of other tables naming it (an account's contacts and deals, a
 * contact's deals at any account) and its history. A related record, or a
 * field naming a record of a table keeping a dimension (a deal's account),
 * opens in the same dialog; Back returns. A new record (record null) opens
 * in its form. Lists and boards open their records here.
 */
export function RecordDialog({ form, forms, record, initialData, revisionId, onClose }: {
  form: FormDef;
  forms: FormDef[];
  record: FormRecord | null;
  initialData?: Record<string, unknown>;
  revisionId: string;
  onClose: () => void;
}) {
  const [stack, setStack] = useState<RecordStep[]>([{ form, record, initialData }]);
  const step = stack[stack.length - 1];
  const push = (next: RecordStep) => setStack(s => [...s, next]);
  const back = () => setStack(s => (s.length > 1 ? s.slice(0, -1) : s));
  const previous = stack.length > 1 ? stack[stack.length - 2] : null;
  const title = step.record ? recordTitle(step.form, step.record) : `New ${singular(step.form.label).toLowerCase()}`;
  return (
    <Dialog open onClose={onClose} title={title} width={step.form.keeps_dimension_id && step.record ? 1040 : 640}>
      {previous && (
        <button type="button" className="mvx-link-button" style={{ marginBottom: 10, fontSize: 12 }} onClick={back}>
          ← Back to {previous.record ? recordTitle(previous.form, previous.record) : `the new ${singular(previous.form.label).toLowerCase()}`}
        </button>
      )}
      <RecordView key={stack.length + (step.record?.id ?? "new")} step={step} forms={forms} revisionId={revisionId}
        onOpen={(f, r) => push({ form: f, record: r })}
        onAdd={(f, field, code) => push({ form: f, record: null, initialData: { [field]: code } })}
        onDone={() => (stack.length > 1 ? back() : onClose())} />
    </Dialog>
  );
}

/** One step of RecordDialog: a record's details, related records and history, or a new record's form. */
function RecordView({ step, forms, revisionId, onOpen, onAdd, onDone }: {
  step: RecordStep;
  forms: FormDef[];
  revisionId: string;
  onOpen: (form: FormDef, record: FormRecord) => void;
  onAdd: (form: FormDef, field: string, code: string) => void;
  onDone: () => void;
}) {
  const { form, initialData } = step;
  const qc = useQueryClient();
  const isNew = !step.record;
  // The record as the server now has it: the list it was opened from may be older.
  const { data: live } = useQuery({
    queryKey: ["records", "one", step.record?.id],
    queryFn: () => api.getRecord(step.record!.id),
    enabled: !!step.record,
    initialData: step.record ?? undefined,
  });
  const record = isNew ? null : (live ?? step.record);
  const canEdit = isNew ? canCreateRecords(form) : !!record?.permissions?.edit;
  const [editing, setEditing] = useState(isNew);
  const [draft, setDraft] = useState<Record<string, unknown>>(() => (record ? { ...record.data } : { ...defaultsOf(form), ...(initialData ?? {}) }));
  const [statusPick, setStatusPick] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dimensions"], queryFn: api.getDimensions });
  const { data: metricsRaw = [] } = useQuery({ queryKey: ["metrics", revisionId], queryFn: () => api.getMetrics(revisionId), enabled: !!revisionId });
  const { data: me } = useQuery({ queryKey: ["me"], queryFn: api.getMe });
  const dims = dimsRaw as DevDimension[];
  const metrics = metricsRaw as Metric[];
  const hasStatus = !!statusFieldOf(form);
  const createStatuses = createStatusesOf(form);
  const statusOptions = record ? recordStatusOptions(record) : { options: [], changeable: false };
  const save = useMutation({
    mutationFn: async () => {
      if (isNew) {
        await api.createRecord(form.id, draft, revisionId || undefined, hasStatus ? createStatusFor(form, statusPick) : undefined);
      } else {
        await api.updateRecord(record!.id, { data: draft, status: statusPick && statusPick !== record!.status ? statusPick : undefined });
      }
    },
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["records"] });
      qc.invalidateQueries({ queryKey: ["dimensions"] });
      invalidateModelData(qc);
      if (isNew) onDone();
      else setEditing(false);
    },
    onError: (e: Error) => setError(e.message),
  });
  // A field naming a member a table keeps opens that record here.
  const openMember = async (f: FormField, code: string) => {
    const keeper = keeperOf(forms, f);
    const codeField = keeper?.fields.find(x => x.type === "autocode");
    if (!keeper || !codeField) return;
    try {
      const res = await api.queryRecords(keeper.id, { filters: [{ field: codeField.name, op: "eq", value: code }], limit: 1 });
      const found = res.records?.[0];
      if (found) onOpen(keeper, found);
      else setError(`That ${singular(keeper.label).toLowerCase()} is not one you can open.`);
    } catch (e) {
      setError((e as Error).message);
    }
  };
  const linkable = (f: FormField) => !!keeperOf(forms, f);

  const details = editing && canEdit ? (
    <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <RecordFields form={form} forms={forms} draft={draft} setDraft={setDraft} dims={dims} metrics={metrics} revisionId={revisionId}
        readOnlyOut={f => postsMember(form, f) && (isNew || (!!me && record?.created_by === me.user_id))} />
      {hasStatus && isNew && createStatuses.length > 1 && (
        <Field label={statusFieldOf(form)?.label || "Status"}>
          <Select value={createStatusFor(form, statusPick)} onChange={e => setStatusPick(e.target.value)} style={{ alignSelf: "flex-start" }}>
            {createStatuses.map(s => <option key={s} value={s}>{s}</option>)}
          </Select>
        </Field>
      )}
      {hasStatus && record && statusOptions.options.length > 0 && (
        <Field label={statusFieldOf(form)?.label || "Status"}>
          <Select value={statusPick ?? record.status} onChange={e => setStatusPick(e.target.value)} disabled={!statusOptions.changeable} style={{ alignSelf: "flex-start" }}>
            {statusOptions.options.map(s => <option key={s} value={s}>{statusLabel(s)}</option>)}
          </Select>
        </Field>
      )}
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <Button variant="primary" loading={save.isPending} loadingLabel="Saving…" onClick={() => { setError(null); save.mutate(); }}>
          {isNew ? "Save" : "Save changes"}
        </Button>
        <Button onClick={() => (isNew ? onDone() : (setEditing(false), setDraft({ ...record!.data })))}>Cancel</Button>
      </div>
    </div>
  ) : record ? (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      <dl className="mvx-record-fields">
        {editableFields(form).map(f => (
          <React.Fragment key={f.name}>
            <dt>{f.label || f.name}</dt>
            <dd><RecordFieldValue form={form} f={f} data={record.data} dims={dims} metrics={metrics}
              onOpenMember={linkable(f) ? openMember : undefined} /></dd>
          </React.Fragment>
        ))}
      </dl>
      {hasStatus && <RecordStatusBadge status={record.status} />}
      <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
        {canEdit && <Button variant="primary" leadingIcon={<Pencil size={13} />} onClick={() => { setDraft({ ...record.data }); setEditing(true); }}>Edit</Button>}
        <span style={{ flex: 1 }} />
        {record.permissions?.delete && (
          <DeleteRecordButton record={record} withPlan={!!form.keeps_dimension_id} variant="button" onDeleted={onDone} onError={setError} />
        )}
      </div>
      <span className="mvx-admin-muted" style={{ fontSize: 11 }}>
        Created {localDateTime(record.created_at)} · updated {localDateTime(record.updated_at)}
      </span>
    </div>
  ) : null;

  const side = record && (form.keeps_dimension_id || form.fields.some(f => f.history)) ? (
    <div style={{ display: "flex", flexDirection: "column", gap: 18, minWidth: 0 }}>
      <RelatedRecordsSection form={form} record={record} forms={forms} dims={dims} metrics={metrics} onOpen={onOpen} onAdd={onAdd} />
      <RecordHistorySection form={form} recordId={record.id} dims={dims} />
    </div>
  ) : null;

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
      {error && <InlineAlert tone="danger">{error}</InlineAlert>}
      <div className={side ? "mvx-record-page" : undefined}>
        <div style={{ minWidth: 0 }}>{details}</div>
        {side}
      </div>
    </div>
  );
}
