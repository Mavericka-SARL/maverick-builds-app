import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { WorkflowDef, WorkflowDefUsage, ContextVariable, WorkflowSubjectType, GridDef, FormDef, DevModel } from "../../api/client";
import { StatusBadge, Field, TriggerEventSelect } from "./WorkflowShared";
import { StatusBadge as Badge } from "../../ui";
import { inputStyle, btnPrimary, btnSecondary, TRIGGER_FALLBACK, useTriggerEvents } from "./workflowConstants";
import { CreateAutomationModal } from "./CreateAutomationModal";

const CONTEXT_DATA_TYPES = [
  "Text", "Number", "Boolean", "Date", "User", "Role",
  "Dimension member", "Metric", "Form record",
];

const SUBJECT_TYPE_OPTIONS: { value: WorkflowSubjectType; label: string; description: string }[] = [
  { value: "",            label: "Not specified",                  description: "General workflow, no linked entity" },
  { value: "grid",        label: "Specific planning grid",         description: "Approve changes in a planning grid" },
  { value: "grid_metric", label: "Metric in a planning grid",      description: "Approve a specific metric value in a grid" },
  { value: "form_records",label: "Selected records in a form",     description: "Approve a set of selected form records" },
  { value: "form_record", label: "Single saved record in a form",  description: "Approve one specific form record" },
];

// ── Properties Panels ─────────────────────────────────────────────────────────

/** The definition's last runs — real status, test runs marked — read-only;
    until now a developer had to use the business admin's History for this. */
function InstancesSection({ workflow }: { workflow: WorkflowDef }) {
  const { data: instances = [], isLoading } = useQuery({
    queryKey: ["workflow-instances", workflow.id],
    queryFn: () => api.getWorkflowInstances(workflow.id),
    refetchInterval: 10_000,
  });
  if (isLoading) return <p className="mvx-admin-muted" style={{ fontSize: 13 }}>Loading instances…</p>;
  if (instances.length === 0) return <p className="mvx-admin-muted" style={{ fontSize: 13 }}>No instances yet. Publish the workflow and start it, or run a test.</p>;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 8 }} aria-label="Workflow instances">
      {instances.map(inst => (
        <div key={inst.id} className="mvx-admin-object" style={{ padding: "8px 10px" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 12 }}>
            <Badge tone={inst.status === "completed" ? "success" : inst.status === "running" ? "warning" : "neutral"}>{inst.status}</Badge>
            {inst.test_run && <Badge tone="draft">test run</Badge>}
            <span className="mvx-admin-muted" style={{ marginLeft: "auto" }}>{new Date(inst.started_at).toLocaleString()}</span>
          </div>
          {inst.context && Object.keys(inst.context).length > 0 && (
            <div className="mvx-admin-muted" style={{ fontSize: 12, marginTop: 4, wordBreak: "break-word" }}>
              {Object.entries(inst.context).map(([k, v]) => `${k}: ${v}`).join(" · ")}
            </div>
          )}
        </div>
      ))}
    </div>
  );
}

/** What the trigger event hands over at start, read-only, in the one place
    context is defined — it used to sit under the trigger picker in
    Properties while the variables lived here, two overlapping lists. */
function TriggerPayloadFields({ applicationId, triggerEvent }: { applicationId: string; triggerEvent: string }) {
  const { data: events = TRIGGER_FALLBACK } = useTriggerEvents(applicationId);
  const selected = events.find(e => e.key === triggerEvent);
  if (!selected || selected.payload_schema.length === 0) return null;
  return (
    <div style={{ marginBottom: 12, background: "var(--color-surface-faint)", border: "1px solid var(--color-border)", borderRadius: 6, padding: "8px 10px" }} aria-label="Provided by the trigger">
      <div style={{ fontSize: 11, fontWeight: 600, color: "var(--color-text-quiet)", marginBottom: 4, textTransform: "uppercase", letterSpacing: "0.05em" }}>
        Provided by the trigger — {selected.label}
      </div>
      {selected.payload_schema.map(f => (
        <div key={f.key} style={{ display: "flex", gap: 6, alignItems: "baseline", fontSize: 12, color: "var(--color-text-strong)", marginBottom: 2 }}>
          <code style={{ background: "var(--color-border)", padding: "1px 5px", borderRadius: 3, fontSize: 11 }}>{f.key}</code>
          <span style={{ color: "var(--color-text-quiet)" }}>{f.type}</span>
          {f.required && <span style={{ color: "var(--color-danger)", fontSize: 11 }}>required</span>}
        </div>
      ))}
    </div>
  );
}

function UsageSection({ workflow, usage }: { workflow: WorkflowDef; usage: WorkflowDefUsage[] }) {
  const [showCreate, setShowCreate] = useState(false);
  return (
    <div>
      {usage.length === 0 ? (
        <div style={{ fontSize: 13, color: "var(--color-disabled)", lineHeight: 1.6, marginBottom: 12 }}>
          This workflow is not started by any trigger yet.
          Create a trigger to start it from a form, button, or API call.
        </div>
      ) : (
        usage.map(u => (
          <div key={u.rule_id} style={{ border: "1px solid var(--color-border)", borderRadius: 8, padding: "10px", marginBottom: 8 }}>
            <div style={{ fontWeight: 600, fontSize: 13, color: "var(--color-text)" }}>{u.rule_name}</div>
            <div style={{ fontSize: 12, color: "var(--color-text-quiet)", marginTop: 2 }}>
              {u.trigger_type} · {u.enabled ? "Enabled" : "Disabled"}
            </div>
          </div>
        ))
      )}
      <button onClick={() => setShowCreate(true)} style={{ ...btnSecondary, fontSize: 12, padding: "6px 12px" }}>
        + Create Trigger
      </button>
      {showCreate && <CreateAutomationModal workflow={workflow} onClose={() => setShowCreate(false)} />}
    </div>
  );
}

function WorkflowObjectPicker({ subjectType, subjectConfig, revisionId, onChange }: {
  subjectType: WorkflowSubjectType | "";
  subjectConfig: Record<string, string>;
  revisionId?: string;
  onChange: (subjectType: WorkflowSubjectType, subjectConfig: Record<string, string>) => void;
}) {
  // Scoped to the revision being edited: without revision_id the grids
  // endpoint returns every revision's grids, so a model with five revisions
  // listed "Sales / Sales Plan" five times over, indistinguishable
  // (reported live, 2026-09-10).
  const gridsQuery = useQuery<GridDef[]>({
    queryKey: ["dev-grids-for-workflow", revisionId],
    queryFn: () => api.listGrids(revisionId),
    staleTime: 60_000,
    enabled: subjectType === "grid" || subjectType === "grid_metric",
  });
  const modelQuery = useQuery<DevModel>({
    queryKey: ["dev-model-for-workflow", revisionId],
    queryFn: () => api.getDevModel(revisionId),
    staleTime: 60_000,
    enabled: subjectType === "grid_metric",
  });
  const formsQuery = useQuery<FormDef[]>({
    queryKey: ["dev-forms-for-workflow", revisionId],
    queryFn: () => api.listForms(revisionId),
    staleTime: 60_000,
    enabled: subjectType === "form_record" || subjectType === "form_records",
  });

  const selectedGrid = gridsQuery.data?.find(g => g.id === subjectConfig.grid_id);
  const metricsInGrid = modelQuery.data?.metrics.filter(m => selectedGrid?.metric_ids.includes(m.id)) ?? [];

  // Switching the object type keeps whatever the new type can still use
  // (grid → grid_metric keeps the grid; form_record ↔ form_records keeps
  // the form) instead of discarding the choice (constructor audit).
  const handleTypeChange = (newType: WorkflowSubjectType) => {
    const kept: Record<string, string> = {};
    if ((newType === "grid" || newType === "grid_metric") && subjectConfig.grid_id) kept.grid_id = subjectConfig.grid_id;
    if (newType === "grid_metric" && subjectConfig.metric_id) kept.metric_id = subjectConfig.metric_id;
    if ((newType === "form_record" || newType === "form_records") && subjectConfig.form_id) kept.form_id = subjectConfig.form_id;
    onChange(newType, kept);
  };

  return (
    <Field label="Workflow object">
      <select
        value={subjectType}
        onChange={e => handleTypeChange(e.target.value as WorkflowSubjectType)}
        style={inputStyle}
      >
        {SUBJECT_TYPE_OPTIONS.map(o => (
          <option key={o.value} value={o.value}>{o.label}</option>
        ))}
      </select>
      {subjectType && (
        <div style={{ fontSize: 11, color: "var(--color-text-quiet)", marginTop: 4 }}>
          {SUBJECT_TYPE_OPTIONS.find(o => o.value === subjectType)?.description}
        </div>
      )}
      {(subjectType === "grid" || subjectType === "grid_metric") && (
        <div style={{ marginTop: 8 }}>
          <select
            value={subjectConfig.grid_id ?? ""}
            onChange={e => onChange(subjectType, { ...subjectConfig, grid_id: e.target.value, metric_id: "" })}
            style={{ ...inputStyle, marginTop: 0 }}
          >
            <option value="">— select grid —</option>
            {(gridsQuery.data ?? []).map(g => (
              <option key={g.id} value={g.id}>{g.name}</option>
            ))}
          </select>
          {subjectType === "grid_metric" && subjectConfig.grid_id && (
            <select
              value={subjectConfig.metric_id ?? ""}
              onChange={e => onChange(subjectType, { ...subjectConfig, metric_id: e.target.value })}
              style={{ ...inputStyle, marginTop: 6 }}
            >
              <option value="">— select metric —</option>
              {metricsInGrid.map(m => (
                <option key={m.id} value={m.id}>{m.label || m.name}</option>
              ))}
            </select>
          )}
        </div>
      )}
      {(subjectType === "form_record" || subjectType === "form_records") && (
        <select
          value={subjectConfig.form_id ?? ""}
          onChange={e => onChange(subjectType, { form_id: e.target.value })}
          style={{ ...inputStyle, marginTop: 8 }}
        >
          <option value="">— select form —</option>
          {(formsQuery.data ?? []).map(f => (
            <option key={f.id} value={f.id}>{f.label || f.name}</option>
          ))}
        </select>
      )}
    </Field>
  );
}

export function WorkflowPropertiesPanel({ workflow, usage, onChange, applicationId, revisionId, activeSection, onSectionChange }: {
  workflow: WorkflowDef;
  usage: WorkflowDefUsage[];
  onChange: (patch: Partial<WorkflowDef>) => void;
  applicationId: string;
  revisionId?: string;
  activeSection: "properties" | "context" | "usage" | "instances";
  onSectionChange: (section: "properties" | "context" | "usage" | "instances") => void;
}) {
  const [newCtxKey, setNewCtxKey] = useState("");
  const { data: ctxDimensions = [] } = useQuery({ queryKey: ["dev-dimensions-all"], queryFn: () => api.getDevDimensions() });

  // A second variable with the same key would be ambiguous for the runtime
  // (and collided as a React key): refuse it instead of listing it twice.
  const duplicateKey = !!newCtxKey.trim() && workflow.context_schema.some(v => v.key === newCtxKey.trim());
  const addContextVar = () => {
    if (!newCtxKey.trim() || duplicateKey) return;
    const newVar: ContextVariable = { key: newCtxKey.trim(), label: newCtxKey.trim(), data_type: "Text", required: false };
    onChange({ context_schema: [...workflow.context_schema, newVar] });
    setNewCtxKey("");
  };

  const updateCtxVar = (idx: number, patch: Partial<ContextVariable>) => {
    const updated = workflow.context_schema.map((v, i) => i === idx ? { ...v, ...patch } : v);
    onChange({ context_schema: updated });
  };

  const removeCtxVar = (idx: number) => {
    onChange({ context_schema: workflow.context_schema.filter((_, i) => i !== idx) });
  };

  return (
    <div>
      {/* Section tabs */}
      <div style={{ display: "flex", borderBottom: "1px solid var(--color-border)" }}>
        {(["properties", "context", "usage", "instances"] as const).map(sec => (
          <button key={sec} onClick={() => onSectionChange(sec)} style={{
            flex: 1, padding: "10px 4px", border: "none", background: activeSection === sec ? "var(--color-surface)" : "var(--color-grid-row-alt-bg)",
            borderBottom: activeSection === sec ? "2px solid var(--color-selected)" : "2px solid transparent",
            fontSize: 12, fontWeight: 600, color: activeSection === sec ? "var(--color-selected)" : "var(--color-text-quiet)", cursor: "pointer",
            textTransform: "capitalize",
          }}>{sec}</button>
        ))}
      </div>

      <div style={{ padding: "16px 14px" }}>
        {activeSection === "properties" && (
          <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
            <Field label="Name">
              <input value={workflow.name} onChange={e => onChange({ name: e.target.value })} style={inputStyle} />
            </Field>
            <Field label="Description">
              <textarea
                value={workflow.description}
                onChange={e => onChange({ description: e.target.value })}
                rows={3}
                style={{ ...inputStyle, resize: "vertical" as const }}
              />
            </Field>
            <Field label="Concurrency">
              <p className="mvx-admin-muted" style={{ margin: "0 0 6px", fontSize: 12 }}>
                One running instance per dimension-member scope: a second start with the same members is refused as a duplicate. Turn off for per-request workflows (an expense request per department, say).
              </p>
              <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13, cursor: "pointer" }}>
                <input
                  type="checkbox"
                  aria-label="One active instance per scope"
                  checked={workflow.single_active_instance !== false}
                  onChange={e => onChange({ single_active_instance: e.target.checked })}
                />
                One active instance per scope
              </label>
            </Field>
            <Field label="Starts on">
              <TriggerEventSelect
                applicationId={applicationId}
                value={workflow.trigger_event}
                onChange={v => onChange({ trigger_event: v })}
              />
            </Field>
            <WorkflowObjectPicker
              subjectType={workflow.subject_type ?? ""}
              subjectConfig={workflow.subject_config ?? {}}
              revisionId={revisionId}
              onChange={(subjectType, subjectConfig) => onChange({ subject_type: subjectType, subject_config: subjectConfig })}
            />
            <div style={{ fontSize: 12, color: "var(--color-disabled)" }}>
              Status: <StatusBadge status={workflow.status} />
            </div>
          </div>
        )}

        {activeSection === "context" && (
          <div>
            <TriggerPayloadFields applicationId={applicationId} triggerEvent={workflow.trigger_event} />
            <p style={{ fontSize: 12, color: "var(--color-text-quiet)", margin: "0 0 12px" }}>
              Variables this workflow needs beyond what the trigger provides — conditions read them, notifications and the inbox show them.
            </p>
            {workflow.context_schema.map((cv, idx) => (
              <div key={cv.key} style={{ border: "1px solid var(--color-border)", borderRadius: 8, padding: "10px", marginBottom: 8 }}>
                <div style={{ display: "flex", alignItems: "center", justifyContent: "space-between", marginBottom: 6 }}>
                  <code style={{ fontSize: 12, color: "var(--color-text-strong)", background: "var(--color-surface-muted)", padding: "2px 6px", borderRadius: 4 }}>{cv.key}</code>
                  <button onClick={() => removeCtxVar(idx)} style={{ background: "none", border: "none", cursor: "pointer", color: "var(--color-disabled)", fontSize: 14 }} title="Remove">✕</button>
                </div>
                <Field label="Label">
                  <input value={cv.label} onChange={e => updateCtxVar(idx, { label: e.target.value })} style={inputStyle} />
                </Field>
                <Field label="Type">
                  <select value={cv.data_type} onChange={e => updateCtxVar(idx, { data_type: e.target.value })} style={inputStyle}>
                    {CONTEXT_DATA_TYPES.map(t => <option key={t} value={t}>{t}</option>)}
                  </select>
                </Field>
                {cv.data_type === "Dimension member" && (
                  <>
                    <Field label="Dimension">
                      <select value={cv.dimension_id ?? ""} onChange={e => updateCtxVar(idx, { dimension_id: e.target.value || undefined })} style={inputStyle}>
                        <option value="">Select a dimension…</option>
                        {ctxDimensions.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
                      </select>
                    </Field>
                    <Field label="Source">
                      <select value={cv.source_hint ?? "manual"} onChange={e => updateCtxVar(idx, { source_hint: e.target.value === "manual" ? undefined : e.target.value })} style={inputStyle}>
                        <option value="manual">Manual entry (caller/widget supplies it)</option>
                        <option value="raci_responsible">Caller's RACI "responsible" scope</option>
                      </select>
                    </Field>
                  </>
                )}
                <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 13, marginTop: 6 }}>
                  <input type="checkbox" checked={cv.required} onChange={e => updateCtxVar(idx, { required: e.target.checked })} />
                  Required
                </label>
              </div>
            ))}
            <div style={{ display: "flex", gap: 6, marginTop: 8 }}>
              <input
                value={newCtxKey}
                onChange={e => setNewCtxKey(e.target.value)}
                onKeyDown={e => e.key === "Enter" && addContextVar()}
                placeholder="variable_key"
                style={{ ...inputStyle, flex: 1, fontFamily: "monospace" }}
              />
              <button onClick={addContextVar} disabled={duplicateKey} title={duplicateKey ? "A variable with this key already exists" : undefined} style={{ ...btnPrimary, opacity: duplicateKey ? 0.5 : 1 }}>Add</button>
            </div>
            {duplicateKey && <div role="alert" style={{ fontSize: 12, color: "var(--color-danger)", marginTop: 6 }}>A variable named <code>{newCtxKey.trim()}</code> already exists.</div>}
          </div>
        )}

        {activeSection === "usage" && (
          <UsageSection workflow={workflow} usage={usage} />
        )}
        {activeSection === "instances" && (
          <InstancesSection workflow={workflow} />
        )}
      </div>
    </div>
  );
}
