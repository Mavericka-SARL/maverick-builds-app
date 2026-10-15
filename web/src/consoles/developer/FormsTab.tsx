import { useState, useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Trash2, Plus, Pencil, Check } from "lucide-react";
import { api, type FormField, type DevDimension, type DevMetric, type FormDef } from "../../api/client";
import { TextInput, NumberInput, Select, Checkbox, IconButton, Button, Toolbar, ToolbarGroup, Field, EmptyState, LoadingState, SearchInput, useConfirm } from "../../ui";

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

function FormFieldsEditor({
  fields, onChange, dims, metrics,
}: {
  fields: FormField[];
  onChange: (f: FormField[]) => void;
  dims: DevDimension[];
  metrics: DevMetric[];
}) {
  const update = (i: number, patch: Partial<FormField>) =>
    onChange(fields.map((f, idx) => idx === i ? { ...f, ...patch } : f));

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
              <IconButton aria-label={`Remove field ${f.label || f.name || i + 1}`} title="Remove field" danger size={26}
                onClick={() => onChange(fields.filter((_, idx) => idx !== i))}>
                <Trash2 size={13} />
              </IconButton>
            </div>
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
                  Each new record gets the next code: {f.code_prefix ?? ""}{"1".padStart(f.code_digits ?? 4, "0")}. Records already saved get codes too.
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

export function FormsTab({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState("");
  const [newLabel, setNewLabel] = useState("");
  const [newFields, setNewFields] = useState<FormField[]>([blankField()]);
  const [newOwnOnly, setNewOwnOnly] = useState(false);
  const [editId, setEditId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editLabel, setEditLabel] = useState("");
  const [editFields, setEditFields] = useState<FormField[]>([]);
  const [editOwnOnly, setEditOwnOnly] = useState(false);
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
  const resetCreate = () => { setNewName(""); setNewLabel(""); setNewFields([blankField()]); setNewOwnOnly(false); };
  const createForm = useMutation({
    mutationFn: () => api.createForm({ name: newName, label: newLabel, fields: newFields, own_records_only: newOwnOnly }, revisionId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["forms"] });
      setShowCreate(false);
      resetCreate();
    },
  });
  const updateForm = useMutation({
    mutationFn: ({ id, name, label, fields, own }: { id: string; name: string; label: string; fields: FormField[]; own: boolean }) =>
      api.updateForm(id, { name, label, fields, own_records_only: own }),
    onSuccess: (res) => {
      qc.invalidateQueries({ queryKey: ["forms"] });
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

  const startEdit = (f: FormDef) => {
    setEditId(f.id); setEditName(f.name); setEditLabel(f.label); setEditFields([...(f.fields ?? [])]); setEditOwnOnly(!!f.own_records_only);
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
          <FormFieldsEditor fields={newFields} onChange={setNewFields} dims={dims} metrics={formMetrics} />
          <div style={{ marginBottom: 16 }}>
            <Checkbox checked={newOwnOnly} onChange={e => setNewOwnOnly(e.target.checked)} label="Each person sees only their own records" />
            <p className="mvx-admin-muted" style={{ fontSize: 12, margin: "4px 0 0 24px" }}>{ownRecordsHint}</p>
          </div>
          <Button variant="primary"
            disabled={!newName || !newLabel || newFields.some(f => !f.name)}
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
                  <FormFieldsEditor fields={editFields} onChange={setEditFields} dims={dims} metrics={formMetrics} />
                  <div>
                    <Checkbox checked={editOwnOnly} onChange={e => setEditOwnOnly(e.target.checked)} label="Each person sees only their own records" />
                    <p className="mvx-admin-muted" style={{ fontSize: 12, margin: "4px 0 0 24px" }}>{ownRecordsHint}</p>
                  </div>
                  {statusChange && (
                    <p className="mvx-admin-muted" style={{ fontSize: 12 }}>
                      {editFields.some(ff => ff.type === "status")
                        ? "With a status field, new records start as drafts and count once their status says so. Records saved so far keep no status and keep counting."
                        : "Without a status field, every record counts as soon as it is saved: saving posts the records that waited for approval into their metrics."}
                    </p>
                  )}
                  <div className="mvx-admin-inline-form">
                    <Button variant="primary"
                      disabled={!!savedOk || !editName || !editLabel}
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
