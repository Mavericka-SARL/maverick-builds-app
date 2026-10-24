import { useState, useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Trash2, Plus, Pencil, Check, ArrowUp, ArrowDown, SlidersHorizontal } from "lucide-react";
import { api, type FormField, type DevDimension, type DevMetric, type FormDef, type EditAccess } from "../../api/client";
import { TextInput, NumberInput, Select, Checkbox, IconButton, Button, Toolbar, ToolbarGroup, Field, EmptyState, LoadingState, SearchInput, useConfirm, Txt } from "../../ui";

const blankField = (): FormField => ({ name: "", label: "", type: "text", required: false });

/** The field-type list: plain types, then the server-filled ones, then the model's dimensions. */
function FieldTypeSelect({ value, onChange, dims, statusTaken }: {
  value: string;
  onChange: (v: string) => void;
  dims: DevDimension[];
  statusTaken: boolean;
}) {
  return (
    <Select value={value} onChange={e => onChange(e.target.value)} style={{ width: 150 }} aria-label="Field type">
      {value === "" && <option value="">— choose —</option>}
      <option value="text">Text</option>
      <option value="longtext">Long text</option>
      <option value="url">Link (URL)</option>
      <option value="number">Number</option>
      <option value="date">Date</option>
      <option value="select">Select</option>
      <option value="boolean">Boolean</option>
      <option value="metric">Metric</option>
      <option value="autocode">Auto code</option>
      {/* A table has at most one status field. */}
      <option value="status" disabled={statusTaken && value !== "status"}>Status</option>
      {dims.length > 0 && (
        <optgroup label="Dimensions">
          {dims.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
        </optgroup>
      )}
    </Select>
  );
}

/** A field's options beyond its type: default, pairing, visibility, history, and per type money, deadline or delete rule. */
function FieldOptions({ f, fields, onUpdate, keeper }: {
  f: FormField;
  fields: FormField[];
  onUpdate: (patch: Partial<FormField>) => void;
  /** The table keeping the dimension a dimension field names, when one does. */
  keeper?: FormDef;
}) {
  const serverFilled = f.type === "autocode" || f.type === "status";
  const others = fields.filter(o => o !== f && o.name && o.type !== "autocode" && o.type !== "status");
  const row = { display: "flex", gap: 10, alignItems: "center", flexWrap: "wrap" as const, fontSize: 12 };
  return (
    <div className="mvx-panel" style={{ marginLeft: 148, marginTop: 6, padding: 10, display: "flex", flexDirection: "column", gap: 8 }} role="group" aria-label={`Options of ${f.label || f.name || "field"}`}>
      {!serverFilled && f.type !== "metric" && (
        <div style={row}>
          <span className="mvx-admin-muted">Default:</span>
          {f.type === "boolean" ? (
            <Select style={{ width: 220 }} value={f.default === undefined ? "" : String(f.default)} aria-label="Default"
              onChange={e => onUpdate({ default: e.target.value === "" ? undefined : e.target.value === "true" })}>
              <option value="">none</option><option value="true">Yes</option><option value="false">No</option>
            </Select>
          ) : f.type === "select" ? (
            <Select style={{ width: 220 }} value={String(f.default ?? "")} aria-label="Default" onChange={e => onUpdate({ default: e.target.value || undefined })}>
              <option value="">none</option>
              {(f.options ?? []).map(o => <option key={o} value={o}>{o}</option>)}
            </Select>
          ) : f.type === "date" ? (
            <Select style={{ width: 220 }} value={f.default === "today" ? "today" : ""} aria-label="Default" onChange={e => onUpdate({ default: e.target.value || undefined })}>
              <option value="">none</option><option value="today">The day it is created</option>
            </Select>
          ) : (
            <TextInput value={String(f.default ?? "")} aria-label="Default" style={{ width: 160 }} placeholder={f.type === "dimension" ? "member code" : ""}
              type={f.type === "number" ? "number" : "text"}
              onChange={e => onUpdate({ default: e.target.value === "" ? undefined : f.type === "number" ? Number(e.target.value) : e.target.value })} />
          )}
          <span className="mvx-admin-muted" style={{ marginLeft: 8 }}>Filled in together with:</span>
          <Select style={{ width: 220 }} value={f.required_with ?? ""} aria-label="Filled in together with" onChange={e => onUpdate({ required_with: e.target.value || undefined })}>
            <option value="">—</option>
            {others.map(o => <option key={o.name} value={o.name}>{o.label || o.name}</option>)}
          </Select>
        </div>
      )}
      <div style={row}>
        <Checkbox checked={!!f.hidden} onChange={e => onUpdate({ hidden: e.target.checked || undefined })} label="Hidden" />
        <Checkbox checked={!!f.core} onChange={e => onUpdate({ core: e.target.checked || undefined })} label="Core (cannot be removed)" />
        {!serverFilled && <Checkbox checked={!!f.history} onChange={e => onUpdate({ history: e.target.checked || undefined })} label="Keep history" />}
        {f.type === "number" && (
          <Checkbox checked={f.format === "money"} onChange={e => onUpdate({ format: e.target.checked ? "money" : undefined })} label="Money" />
        )}
        {f.type === "date" && (
          <Checkbox checked={!!f.deadline} onChange={e => onUpdate({ deadline: e.target.checked || undefined, ...(e.target.checked ? {} : { done_field: undefined, done_values: undefined }) })}
            label="Deadline (overdue once past)" />
        )}
      </div>
      {f.type === "date" && f.deadline && (
        <div style={row}>
          <span className="mvx-admin-muted">Not overdue when</span>
          <Select style={{ width: 220 }} value={f.done_field ?? ""} aria-label="Done field" onChange={e => onUpdate({ done_field: e.target.value || undefined, done_values: e.target.value ? f.done_values : undefined })}>
            <option value="">— always overdue once past —</option>
            {others.map(o => <option key={o.name} value={o.name}>{o.label || o.name}</option>)}
          </Select>
          {f.done_field && (
            <>
              <span className="mvx-admin-muted">is one of</span>
              <TextInput value={(f.done_values ?? []).join(", ")} aria-label="Done values" style={{ width: 180 }} placeholder="Completed, Cancelled"
                onChange={e => onUpdate({ done_values: e.target.value.split(",").map(v => v.trim()).filter(Boolean) })} />
            </>
          )}
        </div>
      )}
      {f.type === "dimension" && keeper && (
        <div style={row}>
          <span className="mvx-admin-muted">Links to a record of {keeper.label}. When that record is deleted:</span>
          <Select style={{ width: 220 }} value={f.on_delete ?? "block"} aria-label="When the linked record is deleted"
            onChange={e => onUpdate({ on_delete: e.target.value === "block" ? undefined : e.target.value as FormField["on_delete"] })}>
            <option value="block">refuse while this record names it</option>
            <option value="cascade">delete this record with it</option>
            <option value="clear" disabled={f.required}>empty this field</option>
          </Select>
        </div>
      )}
    </div>
  );
}

function FormFieldsEditor({
  fields, onChange, dims, metrics, forms = [],
}: {
  fields: FormField[];
  onChange: (f: FormField[]) => void;
  dims: DevDimension[];
  metrics: DevMetric[];
  /** The revision's tables: a dimension field naming a dimension one keeps links to its records. */
  forms?: FormDef[];
}) {
  const [optionsOpen, setOptionsOpen] = useState<Set<number>>(new Set());
  const update = (i: number, patch: Partial<FormField>) =>
    onChange(fields.map((f, idx) => idx === i ? { ...f, ...patch } : f));
  const moveField = (i: number, by: number) => {
    const j = i + by;
    if (j < 0 || j >= fields.length) return;
    const next = [...fields];
    [next[i], next[j]] = [next[j], next[i]];
    onChange(next);
  };

  const inputMetrics = metrics.filter(m => m.is_input);
  const statusTaken = fields.some(f => f.type === "status");

  return (
    <>
      {fields.map((f, i) => {
        const typeValue = f.type === "dimension" ? (f.dimension_id ?? "") : f.type;
        const reset = { dimension_id: undefined, metric_id: undefined, allowed_members: undefined, value_field: undefined, code_prefix: undefined, code_digits: undefined };

        const handleTypeChange = (val: string) => {
          if (dims.some(d => d.id === val))
            update(i, { ...reset, type: "dimension", dimension_id: val });
          else if (val === "autocode")
            update(i, { ...reset, type: "autocode", required: false, code_prefix: "", code_digits: 4 });
          else if (val === "status")
            update(i, { ...reset, type: "status", required: false, name: f.name || "status", label: f.label || "Status" });
          else
            update(i, { ...reset, type: val as FormField["type"] });
        };
        // The server fills an auto code and a workflow sets the status: neither is "required" of a person.
        const serverFilled = f.type === "autocode" || f.type === "status";

        return (
          <div key={i} style={{ marginBottom: 12 }}>
            <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
              <TextInput value={f.name} onChange={e => update(i, { name: e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_") })}
                placeholder="field_name" style={{ width: 140 }} aria-label="Field name" />
              <TextInput value={f.label} onChange={e => update(i, { label: e.target.value })}
                placeholder="Field label" style={{ flex: 1 }} aria-label="Field label" />
              <FieldTypeSelect value={typeValue} onChange={handleTypeChange} dims={dims} statusTaken={statusTaken} />
              <Checkbox
                checked={f.required && !serverFilled}
                disabled={serverFilled}
                onChange={e => update(i, { required: e.target.checked })}
                label="Required"
              />
              <IconButton aria-label={`Options of field ${f.label || f.name || i + 1}`} title="Options" size={26}
                aria-expanded={optionsOpen.has(i)}
                onClick={() => setOptionsOpen(s => { const n = new Set(s); if (n.has(i)) n.delete(i); else n.add(i); return n; })}>
                <SlidersHorizontal size={13} />
              </IconButton>
              <IconButton aria-label={`Move field ${f.label || f.name || i + 1} up`} title="Move up" size={26} disabled={i === 0} onClick={() => moveField(i, -1)}>
                <ArrowUp size={13} />
              </IconButton>
              <IconButton aria-label={`Move field ${f.label || f.name || i + 1} down`} title="Move down" size={26} disabled={i === fields.length - 1} onClick={() => moveField(i, 1)}>
                <ArrowDown size={13} />
              </IconButton>
              <IconButton aria-label={`Remove field ${f.label || f.name || i + 1}`} title={f.core ? "A core field cannot be removed" : "Remove field"} danger size={26}
                disabled={!!f.core}
                onClick={() => onChange(fields.filter((_, idx) => idx !== i))}>
                <Trash2 size={13} />
              </IconButton>
            </div>
            {optionsOpen.has(i) && (
              <FieldOptions f={f} fields={fields} onUpdate={patch => update(i, patch)}
                keeper={f.type === "dimension" ? forms.find(t => t.keeps_dimension_id === f.dimension_id) : undefined} />
            )}
            {f.type === "autocode" && (
              <div style={{ marginLeft: 148, marginTop: 6, display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap", fontSize: 12 }}>
                <span className="mvx-admin-muted">Prefix:</span>
                <TextInput value={f.code_prefix ?? ""} onChange={e => update(i, { code_prefix: e.target.value.slice(0, 20) })}
                  placeholder="EXP-" style={{ width: 90 }} aria-label="Code prefix" />
                <span className="mvx-admin-muted">Digits:</span>
                <NumberInput value={String(f.code_digits ?? 4)} min={1} max={12}
                  onChange={e => update(i, { code_digits: Math.min(12, Math.max(1, parseInt(e.target.value, 10) || 4)) })}
                  style={{ width: 64 }} aria-label="Code digits" />
                <span className="mvx-admin-muted">
                  Each new record gets the next code: <Txt>{f.code_prefix ?? ""}</Txt><Txt>{"1".padStart(f.code_digits ?? 4, "0")}</Txt>. Records already saved get codes too.
                </span>
              </div>
            )}
            {f.type === "status" && (
              <p className="mvx-admin-muted" style={{ marginLeft: 148, marginTop: 6, fontSize: 12 }}>
                Records go through approval: a new record is a draft, and a workflow whose subject is this table moves
                it — submitted while it runs, approved or rejected by its decision. Integrations post the records in
                the statuses they name. Without a status field, every record counts as soon as it is saved.
              </p>
            )}
            {f.type === "metric" && (
              <div style={{ marginLeft: 148, marginTop: 6, display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap", fontSize: 12 }}>
                <span className="mvx-admin-muted">Metric:</span>
                <label style={{ display: "flex", alignItems: "center", gap: 4 }}>
                  <input type="radio" name={`metric_ref_${i}`} checked={!f.metric_id}
                    onChange={() => update(i, { metric_id: undefined })} />
                  By name (dynamic)
                </label>
                <label style={{ display: "flex", alignItems: "center", gap: 4 }}>
                  <input type="radio" name={`metric_ref_${i}`} checked={!!f.metric_id}
                    onChange={() => update(i, { metric_id: inputMetrics[0]?.id ?? "" })} />
                  By ID (fixed)
                </label>
                {f.metric_id ? (
                  <Select value={f.metric_id} onChange={e => update(i, { metric_id: e.target.value })} aria-label="Metric">
                    <option value="">— choose metric —</option>
                    {inputMetrics.map(m => <option key={m.id} value={m.id}>{m.label || m.name}</option>)}
                  </Select>
                ) : (
                  <>
                    <span className="mvx-admin-muted" style={{ marginLeft: 8 }}>Amount field:</span>
                    <Select value={f.value_field ?? ""} onChange={e => update(i, { value_field: e.target.value || undefined })} aria-label="Amount field">
                      <option value="">— none —</option>
                      {fields.filter((_, idx) => idx !== i && _.type === "number").map(nf => (
                        <option key={nf.name} value={nf.name}>{nf.label || nf.name}</option>
                      ))}
                    </Select>
                  </>
                )}
              </div>
            )}
          </div>
        );
      })}
      <Button size="sm" variant="ghost" leadingIcon={<Plus size={13} />} style={{ marginBottom: 16 }}
        onClick={() => onChange([...fields, blankField()])}>
        Add field
      </Button>
    </>
  );
}

const ownRecordsHint = "People who do not administer the application see only the records they created; business admins see every record.";

/** A table's settings beyond its fields: who may change its records, and the dimension whose members they are. */
function TableSettingsEditor({ editAccess, onEditAccess, keeps, onKeeps, labelField, onLabelField, rootLabel, onRootLabel, fields, dims, forms, selfId }: {
  editAccess: EditAccess;
  onEditAccess: (v: EditAccess) => void;
  keeps: string;
  onKeeps: (v: string) => void;
  labelField: string;
  onLabelField: (v: string) => void;
  rootLabel: string;
  onRootLabel: (v: string) => void;
  fields: FormField[];
  dims: DevDimension[];
  forms: FormDef[];
  selfId?: string;
}) {
  const hasCode = fields.some(f => f.type === "autocode");
  const labelFields = fields.filter(f => f.name && (f.type === "text" || f.type === "longtext" || f.type === "select"));
  const keptElsewhere = new Set(forms.filter(t => t.id !== selfId && t.keeps_dimension_id).map(t => t.keeps_dimension_id!));
  const keepable = dims.filter(d => d.dimension_type !== "time" && !keptElsewhere.has(d.id));
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10, marginBottom: 16 }}>
      <Field label="Who may change and delete records"
        description={editAccess === "everyone"
          ? "Everyone who sees a record. A member read-only for a person (an account, a stage) makes the records naming it theirs to see only."
          : "Their creator, while it is a draft or submitted, and the application's administrators."}>
        <Select value={editAccess} onChange={e => onEditAccess(e.target.value as EditAccess)} style={{ width: 320 }}>
          <option value="creator">Their creator and administrators</option>
          <option value="everyone">Everyone who sees them</option>
        </Select>
      </Field>
      <Field label="Records are the members of a dimension"
        description={hasCode
          ? "Each record is one member: its auto code is the member's code, the label field its name. Access rules on the members restrict who sees which records — and every record of another table naming them — and other tables link to a record with a field on the dimension."
          : "Add an Auto code field first: its codes are the members' codes."}>
        <div style={{ display: "flex", gap: 8, flexWrap: "wrap" }}>
          <Select value={keeps} onChange={e => onKeeps(e.target.value)} disabled={!hasCode} style={{ width: 220 }} aria-label="Dimension the records are">
            <option value="">— none —</option>
            {keepable.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
          </Select>
          {keeps && (
            <Select value={labelField} onChange={e => onLabelField(e.target.value)} style={{ width: 220 }} aria-label="Field naming each member">
              <option value="">— field naming each member —</option>
              {labelFields.map(f => <option key={f.name} value={f.name}>{f.label || f.name}</option>)}
            </Select>
          )}
        </div>
      </Field>
      {keeps && (
        <Field label="Total member" description="Optional: a member every record's member sits under (All accounts) — what a dashboard selector opens on.">
          <TextInput value={rootLabel} onChange={e => onRootLabel(e.target.value)} placeholder="All accounts" style={{ width: 220 }} />
        </Field>
      )}
    </div>
  );
}

export function FormsTab({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState("");
  const [newLabel, setNewLabel] = useState("");
  const [newFields, setNewFields] = useState<FormField[]>([blankField()]);
  const [newOwnOnly, setNewOwnOnly] = useState(false);
  const [newEditAccess, setNewEditAccess] = useState<EditAccess>("creator");
  const [newKeeps, setNewKeeps] = useState("");
  const [newLabelField, setNewLabelField] = useState("");
  const [newRootLabel, setNewRootLabel] = useState("");
  const [editId, setEditId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editLabel, setEditLabel] = useState("");
  const [editFields, setEditFields] = useState<FormField[]>([]);
  const [editOwnOnly, setEditOwnOnly] = useState(false);
  const [editEditAccess, setEditEditAccess] = useState<EditAccess>("creator");
  const [editKeeps, setEditKeeps] = useState("");
  const [editLabelField, setEditLabelField] = useState("");
  const [editRootLabel, setEditRootLabel] = useState("");
  const [savedOk, setSavedOk] = useState<string | null>(null);
  const [search, setSearch] = useState("");

  const { data: forms = [], isLoading } = useQuery({ queryKey: ["forms", revisionId], queryFn: () => api.listForms(revisionId) });
  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dev-dimensions-all", revisionId], queryFn: () => api.getDevDimensions(revisionId) });
  const { data: formMetricsRaw } = useQuery({ queryKey: ["dev-model", revisionId], queryFn: () => api.getDevModel(revisionId) });
  const formMetrics: DevMetric[] = (formMetricsRaw as { metrics?: DevMetric[] } | undefined)?.metrics ?? [];
  // Deduplicate by name: base dims (created first, revision_id IS NULL) come first by created_at
  const dims = useMemo(() => {
    const seen = new Set<string>();
    return (dimsRaw as DevDimension[]).filter(d => { if (seen.has(d.name)) return false; seen.add(d.name); return true; });
  }, [dimsRaw]);
  const resetCreate = () => { setNewName(""); setNewLabel(""); setNewFields([blankField()]); setNewOwnOnly(false); setNewEditAccess("creator"); setNewKeeps(""); setNewLabelField(""); setNewRootLabel(""); };
  const createForm = useMutation({
    mutationFn: () => api.createForm({
      name: newName, label: newLabel, fields: newFields, own_records_only: newOwnOnly,
      // Settings left at their defaults are not sent: the request is what it always was.
      ...(newEditAccess !== "creator" ? { edit_access: newEditAccess } : {}),
      ...(newKeeps ? { keeps_dimension_id: newKeeps, member_label_field: newLabelField, ...(newRootLabel.trim() ? { member_root_label: newRootLabel.trim() } : {}) } : {}),
    }, revisionId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["forms"] });
      setShowCreate(false);
      resetCreate();
    },
  });
  const updateForm = useMutation({
    mutationFn: ({ id, name, label, fields, own }: { id: string; name: string; label: string; fields: FormField[]; own: boolean }) =>
      api.updateForm(id, { name, label, fields, own_records_only: own, ...changedSettings(id) }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["forms"] });
      qc.invalidateQueries({ queryKey: ["dev-dimensions-all"] });
      qc.invalidateQueries({ queryKey: ["records"] });
      const notes = [
        res.codes_assigned ? `${res.codes_assigned} existing record(s) given a code` : "",
        res.status_field_changed ? "records are being posted again" : "",
      ].filter(Boolean);
      setSavedOk(notes.length > 0 ? `Saved — ${notes.join("; ")}` : "Saved");
      setTimeout(() => { setSavedOk(null); setEditId(null); }, notes.length > 0 ? 3500 : 1500);
    },
  });
  const deleteForm = useMutation({
    mutationFn: (id: string) => api.deleteForm(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["forms"] }),
  });
  const { confirm, confirmElement } = useConfirm();

  // The table settings the editor changed, to send with the save: none when
  // they are as the table has them, so the request is what it always was.
  const changedSettings = (id: string) => {
    const t = (forms as FormDef[]).find(f => f.id === id);
    const out: { edit_access?: EditAccess; keeps_dimension_id?: string; member_label_field?: string; member_root_label?: string } = {};
    if (editEditAccess !== (t?.edit_access ?? "creator")) out.edit_access = editEditAccess;
    if (editKeeps !== (t?.keeps_dimension_id ?? "") || editLabelField !== (t?.member_label_field ?? "") || editRootLabel.trim() !== (t?.member_root_label ?? "")) {
      out.keeps_dimension_id = editKeeps;
      out.member_label_field = editKeeps ? editLabelField : "";
      out.member_root_label = editKeeps ? editRootLabel.trim() : "";
    }
    return out;
  };
  const startEdit = (f: FormDef) => {
    setEditId(f.id); setEditName(f.name); setEditLabel(f.label); setEditFields([...(f.fields ?? [])]); setEditOwnOnly(!!f.own_records_only);
    setEditEditAccess(f.edit_access ?? "creator"); setEditKeeps(f.keeps_dimension_id ?? ""); setEditLabelField(f.member_label_field ?? ""); setEditRootLabel(f.member_root_label ?? "");
  };
  const editing = (forms as FormDef[]).find(f => f.id === editId);
  // Removing the status field makes every record count; adding one makes new records wait for approval.
  const statusChange = editing ? editing.fields.some(f => f.type === "status") !== editFields.some(f => f.type === "status") : false;

  // A table matches by its name, its label or any field's name or label; the
  // one being edited stays in view whatever its name becomes.
  const q = search.trim().toLowerCase();
  const shownForms = (forms as FormDef[]).filter(f => !q || f.id === editId
    || [f.name, f.label, ...(f.fields ?? []).flatMap(ff => [ff.name, ff.label])].some(v => v?.toLowerCase().includes(q)));

  if (isLoading) return <LoadingState />;

  const describe = (f: FormDef) => {
    const n = (f.fields ?? []).length;
    const parts = [`${n} field${n !== 1 ? "s" : ""}`];
    if ((f.fields ?? []).some(ff => ff.type === "status")) parts.push("records have a status");
    if (f.own_records_only) parts.push("each person sees their own records");
    if (f.edit_access === "everyone") parts.push("everyone who sees a record may edit it");
    if (f.keeps_dimension_id) parts.push(`its records are the members of ${dims.find(d => d.id === f.keeps_dimension_id)?.name ?? "a dimension"}`);
    return parts.join(" · ");
  };

  return (
    <div>
      <Toolbar className="mvx-toolbar--spaced">
        <ToolbarGroup>
          <span className="mvx-admin-muted">
            Tables hold the records people save — requests, expenses, deals. Show one on a dashboard with a Records widget.
          </span>
        </ToolbarGroup>
        <ToolbarGroup align="end">
          <SearchInput value={search} onChange={e => setSearch(e.target.value)}
            placeholder="Search tables and fields…" width={220} />
          <Button leadingIcon={showCreate ? undefined : <Plus size={14} />}
            onClick={() => { setShowCreate(v => !v); resetCreate(); }}>
            {showCreate ? "Cancel" : "New table"}
          </Button>
        </ToolbarGroup>
      </Toolbar>

      {showCreate && (
        <div className="mvx-panel" style={{ padding: 20, marginBottom: 24 }}>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, marginBottom: 16 }}>
            <Field label="Table name" description="snake_case">
              <TextInput value={newName} onChange={e => setNewName(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))}
                placeholder="purchase_request" />
            </Field>
            <Field label="Display label">
              <TextInput value={newLabel} onChange={e => setNewLabel(e.target.value)} placeholder="Purchase Request" />
            </Field>
          </div>
          <div className="mvx-admin-revisions__label" style={{ marginBottom: 10 }}>Fields</div>
          <FormFieldsEditor fields={newFields} onChange={setNewFields} dims={dims} metrics={formMetrics} forms={forms as FormDef[]} />
          <div style={{ marginBottom: 16 }}>
            <Checkbox checked={newOwnOnly} onChange={e => setNewOwnOnly(e.target.checked)} label="Each person sees only their own records" />
            <p className="mvx-admin-muted" style={{ fontSize: 12, margin: "4px 0 0 24px" }}>{ownRecordsHint}</p>
          </div>
          <TableSettingsEditor editAccess={newEditAccess} onEditAccess={setNewEditAccess} keeps={newKeeps} onKeeps={setNewKeeps}
            labelField={newLabelField} onLabelField={setNewLabelField} rootLabel={newRootLabel} onRootLabel={setNewRootLabel} fields={newFields} dims={dims} forms={forms as FormDef[]} />
          <Button variant="primary"
            disabled={!newName || !newLabel || newFields.some(f => !f.name) || (!!newKeeps && !newLabelField)}
            loading={createForm.isPending}
            loadingLabel="Creating…"
            onClick={() => createForm.mutate()}>
            Create table
          </Button>
          {createForm.isError && <p className="mvx-admin-error" style={{ marginTop: 8 }}>{(createForm.error as Error).message}</p>}
        </div>
      )}

      {(forms as FormDef[]).length === 0 ? (
        <EmptyState label="No tables yet. Use the button above to build your first table." />
      ) : shownForms.length === 0 ? (
        <EmptyState label={`No tables match "${search.trim()}".`} />
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          {shownForms.map(f => (
            <div key={f.id} className="mvx-admin-object">
              {editId === f.id ? (
                <div className="mvx-admin-object__body">
                  <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
                    <Field label="Table name">
                      <TextInput value={editName} onChange={e => setEditName(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))} />
                    </Field>
                    <Field label="Display label">
                      <TextInput value={editLabel} onChange={e => setEditLabel(e.target.value)} />
                    </Field>
                  </div>
                  <div className="mvx-admin-revisions__label">Fields</div>
                  <FormFieldsEditor fields={editFields} onChange={setEditFields} dims={dims} metrics={formMetrics} forms={forms as FormDef[]} />
                  <div>
                    <Checkbox checked={editOwnOnly} onChange={e => setEditOwnOnly(e.target.checked)} label="Each person sees only their own records" />
                    <p className="mvx-admin-muted" style={{ fontSize: 12, margin: "4px 0 0 24px" }}>{ownRecordsHint}</p>
                  </div>
                  <TableSettingsEditor editAccess={editEditAccess} onEditAccess={setEditEditAccess} keeps={editKeeps} onKeeps={setEditKeeps}
                    labelField={editLabelField} onLabelField={setEditLabelField} rootLabel={editRootLabel} onRootLabel={setEditRootLabel} fields={editFields} dims={dims} forms={forms as FormDef[]} selfId={f.id} />
                  {statusChange && (
                    <p className="mvx-admin-muted" style={{ fontSize: 12 }}>
                      {editFields.some(ff => ff.type === "status")
                        ? "With a status field, new records start as drafts and count once their status says so. Records saved so far keep no status and keep counting."
                        : "Without a status field, every record counts as soon as it is saved: saving posts the records that waited for approval into their metrics."}
                    </p>
                  )}
                  <div className="mvx-admin-inline-form">
                    <Button variant="primary"
                      disabled={!!savedOk || !editName || !editLabel || (!!editKeeps && !editLabelField)}
                      loading={updateForm.isPending}
                      loadingLabel="Saving…"
                      onClick={() => updateForm.mutate({ id: f.id, name: editName, label: editLabel, fields: editFields, own: editOwnOnly })}>
                      Save
                    </Button>
                    <Button onClick={() => { setSavedOk(null); setEditId(null); }}>Cancel</Button>
                    {savedOk && (
                      <span style={{ color: "var(--color-success)", fontSize: 13, fontWeight: 500, display: "inline-flex", alignItems: "center", gap: 4 }}>
                        <Check size={14} /> {savedOk}
                      </span>
                    )}
                    {!savedOk && updateForm.isError && (
                      <span className="mvx-admin-error">{(updateForm.error as Error).message}</span>
                    )}
                  </div>
                </div>
              ) : (
                <div className="mvx-admin-object__header" style={{ borderBottom: "none", background: "var(--color-surface)" }}>
                  <div className="mvx-admin-object__title">
                    <div className="mvx-admin-object__name">{f.label || f.name}</div>
                    <div className="mvx-admin-mono mvx-admin-muted">{f.name}</div>
                    <div className="mvx-admin-object__meta" style={{ marginTop: 4 }}>
                      {describe(f)}
                      {(f.fields ?? []).length > 0 && (
                        <span style={{ marginLeft: 8 }}>
                          {(f.fields ?? []).map(ff => ff.label || ff.name).join(", ")}
                        </span>
                      )}
                    </div>
                  </div>
                  <Button size="sm" variant="ghost" leadingIcon={<Pencil size={13} />} onClick={() => startEdit(f)}>
                    Edit
                  </Button>
                  <IconButton aria-label={`Delete table ${f.label || f.name}`} title="Delete table" danger
                    onClick={() => confirm({ title: "Delete table?", body: `This removes "${f.label || f.name}" and all its records.`, confirmLabel: "Delete table", onConfirm: () => deleteForm.mutate(f.id) })}>
                    <Trash2 size={14} />
                  </IconButton>
                </div>
              )}
            </div>
          ))}
        </div>
      )}
      {confirmElement}
    </div>
  );
}
