import { useState, useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Trash2, Plus, Pencil, Check } from "lucide-react";
import { api, type FormField, type DevDimension, type DevMetric, type FormDef } from "../../api/client";
import { TextInput, Select, Checkbox, IconButton, Button, Toolbar, ToolbarGroup, Field, EmptyState, LoadingState, useConfirm } from "../../ui";

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

  return (
    <>
      {fields.map((f, i) => {
        const typeValue = f.type === "dimension" ? (f.dimension_id ?? "") : f.type;

        const handleTypeChange = (val: string) => {
          if (dims.some(d => d.id === val))
            update(i, { type: "dimension", dimension_id: val, metric_id: undefined, allowed_members: undefined, value_field: undefined });
          else
            update(i, { type: val as FormField["type"], dimension_id: undefined, metric_id: undefined, allowed_members: undefined, value_field: undefined });
        };

        return (
          <div key={i} style={{ marginBottom: 12 }}>
            <div className="mvx-admin-inline-form" style={{ flexWrap: "nowrap" }}>
              <TextInput value={f.name} onChange={e => update(i, { name: e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_") })}
                placeholder="field_name" style={{ width: 140 }} />
              <TextInput value={f.label} onChange={e => update(i, { label: e.target.value })}
                placeholder="Field label" style={{ flex: 1 }} />
              <Select value={typeValue} onChange={e => handleTypeChange(e.target.value)} style={{ width: 150 }} aria-label="Field type">
                {typeValue === "" && <option value="">— choose —</option>}
                <option value="text">Text</option>
                <option value="number">Number</option>
                <option value="date">Date</option>
                <option value="select">Select</option>
                <option value="boolean">Boolean</option>
                <option value="metric">Metric</option>
                {dims.length > 0 && (
                  <optgroup label="Dimensions">
                    {dims.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
                  </optgroup>
                )}
              </Select>
              <Checkbox
                checked={f.required}
                onChange={e => update(i, { required: e.target.checked })}
                label="Required"
              />
              <IconButton aria-label={`Remove field ${f.label || f.name || i + 1}`} title="Remove field" danger size={26}
                onClick={() => onChange(fields.filter((_, idx) => idx !== i))}>
                <Trash2 size={13} />
              </IconButton>
            </div>
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
        onClick={() => onChange([...fields, { name: "", label: "", type: "text", required: false }])}>
        Add field
      </Button>
    </>
  );
}

export function FormsTab({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const [showCreate, setShowCreate] = useState(false);
  const [newName, setNewName] = useState("");
  const [newLabel, setNewLabel] = useState("");
  const [newFields, setNewFields] = useState<FormField[]>([{ name: "", label: "", type: "text", required: false }]);
  const [editId, setEditId] = useState<string | null>(null);
  const [editName, setEditName] = useState("");
  const [editLabel, setEditLabel] = useState("");
  const [editFields, setEditFields] = useState<FormField[]>([]);
  const [savedOk, setSavedOk] = useState(false);

  const { data: forms = [], isLoading } = useQuery({ queryKey: ["forms", revisionId], queryFn: () => api.listForms(revisionId) });
  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dev-dimensions-all", revisionId], queryFn: () => api.getDevDimensions(revisionId) });
  const { data: formMetricsRaw } = useQuery({ queryKey: ["dev-model", revisionId], queryFn: () => api.getDevModel(revisionId) });
  const formMetrics: DevMetric[] = (formMetricsRaw as { metrics?: DevMetric[] } | undefined)?.metrics ?? [];
  // Deduplicate by name: base dims (created first, revision_id IS NULL) come first by created_at
  const dims = useMemo(() => {
    const seen = new Set<string>();
    return (dimsRaw as DevDimension[]).filter(d => { if (seen.has(d.name)) return false; seen.add(d.name); return true; });
  }, [dimsRaw]);
  const createForm = useMutation({
    mutationFn: () => api.createForm({ name: newName, label: newLabel, fields: newFields }, revisionId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["forms"] });
      setShowCreate(false); setNewName(""); setNewLabel(""); setNewFields([{ name: "", label: "", type: "text", required: false }]);
    },
  });
  const updateForm = useMutation({
    mutationFn: ({ id, name, label, fields }: { id: string; name: string; label: string; fields: FormField[] }) =>
      api.updateForm(id, { name, label, fields }),
    onSuccess: (_, { id, name, label, fields }) => {
      qc.setQueryData(["forms"], (old: FormDef[] | undefined) =>
        (old ?? []).map((f: FormDef) => f.id === id ? { ...f, name, label, fields } : f)
      );
      qc.invalidateQueries({ queryKey: ["forms"] });
      setSavedOk(true);
      setTimeout(() => { setSavedOk(false); setEditId(null); }, 1500);
    },
  });
  const deleteForm = useMutation({
    mutationFn: (id: string) => api.deleteForm(id),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["forms"] }),
  });
  const { confirm, confirmElement } = useConfirm();

  const startEdit = (f: FormDef) => { setEditId(f.id); setEditName(f.name); setEditLabel(f.label); setEditFields([...(f.fields ?? [])]); };

  if (isLoading) return <LoadingState />;

  return (
    <div>
      <Toolbar className="mvx-toolbar--spaced">
        <ToolbarGroup>
          <span className="mvx-admin-muted">
            Forms appear in CRUD apps and let business users submit structured records.
          </span>
        </ToolbarGroup>
        <ToolbarGroup align="end">
          <Button leadingIcon={showCreate ? undefined : <Plus size={14} />}
            onClick={() => { setShowCreate(v => !v); setNewName(""); setNewLabel(""); setNewFields([{ name: "", label: "", type: "text", required: false }]); }}>
            {showCreate ? "Cancel" : "New form"}
          </Button>
        </ToolbarGroup>
      </Toolbar>

      {showCreate && (
        <div className="mvx-panel" style={{ padding: 20, marginBottom: 24 }}>
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, marginBottom: 16 }}>
            <Field label="Form name" description="snake_case">
              <TextInput value={newName} onChange={e => setNewName(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))}
                placeholder="purchase_request" />
            </Field>
            <Field label="Display label">
              <TextInput value={newLabel} onChange={e => setNewLabel(e.target.value)} placeholder="Purchase Request" />
            </Field>
          </div>
          <div className="mvx-admin-revisions__label" style={{ marginBottom: 10 }}>Fields</div>
          <FormFieldsEditor fields={newFields} onChange={setNewFields} dims={dims} metrics={formMetrics} />
          <Button variant="primary"
            disabled={!newName || !newLabel || newFields.some(f => !f.name)}
            loading={createForm.isPending}
            loadingLabel="Creating…"
            onClick={() => createForm.mutate()}>
            Create form
          </Button>
          {createForm.isError && <p className="mvx-admin-error" style={{ marginTop: 8 }}>{(createForm.error as Error).message}</p>}
        </div>
      )}

      {(forms as FormDef[]).length === 0 ? (
        <EmptyState label="No forms yet. Use the button above to build your first form." />
      ) : (
        <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
          {(forms as FormDef[]).map(f => (
            <div key={f.id} className="mvx-admin-object">
              {editId === f.id ? (
                <div className="mvx-admin-object__body">
                  <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
                    <Field label="Form name">
                      <TextInput value={editName} onChange={e => setEditName(e.target.value.toLowerCase().replace(/[^a-z0-9_]/g, "_"))} />
                    </Field>
                    <Field label="Display label">
                      <TextInput value={editLabel} onChange={e => setEditLabel(e.target.value)} />
                    </Field>
                  </div>
                  <div className="mvx-admin-revisions__label">Fields</div>
                  <FormFieldsEditor fields={editFields} onChange={setEditFields} dims={dims} metrics={formMetrics} />
                  <div className="mvx-admin-inline-form">
                    <Button variant="primary"
                      disabled={savedOk || !editName || !editLabel}
                      loading={updateForm.isPending}
                      loadingLabel="Saving…"
                      onClick={() => updateForm.mutate({ id: f.id, name: editName, label: editLabel, fields: editFields })}>
                      Save
                    </Button>
                    <Button onClick={() => { setSavedOk(false); setEditId(null); }}>Cancel</Button>
                    {savedOk && (
                      <span style={{ color: "var(--color-success)", fontSize: 13, fontWeight: 500, display: "inline-flex", alignItems: "center", gap: 4 }}>
                        <Check size={14} /> Saved
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
                      {(f.fields ?? []).length} field{(f.fields ?? []).length !== 1 ? "s" : ""}
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
                  <IconButton aria-label={`Delete form ${f.label || f.name}`} title="Delete form" danger
                    onClick={() => confirm({ title: "Delete form?", body: `This removes "${f.label || f.name}" and all its submitted records.`, confirmLabel: "Delete form", onConfirm: () => deleteForm.mutate(f.id) })}>
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
