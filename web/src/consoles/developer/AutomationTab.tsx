import React, { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { Plus, Pause, Play, Pencil, Trash2 } from "lucide-react";
import { api, type AutomationRule, type Execution, type FormDef, type GridDef, type WorkflowDefSummary , type IntegrationDef } from "../../api/client";
import { SectionHeader, Button, EmptyState, StatusBadge, IconButton, Field, TextInput, Select, useConfirm, type DesignTone } from "../../ui";

const EXEC_STATUS_TONE: Record<string, DesignTone> = {
  completed: "success", running: "warning", failed: "danger", cancelled: "neutral",
};

type RuleState = { name: string; description: string; trigger_type: string; workflow_name: string; workflow_def_id: string; source_form_id: string; source_grid_id: string; source_integration_id: string };

// The workflow's trigger event (a catalog key) → the rule trigger type the
// engine dispatches on. Per-form keys ("expense_request.submitted") and
// per-integration keys ("actuals_csv.import.completed") used to fall
// through to "manual", so a rule created from such a workflow never fired
// on the event it was designed for.
function triggerTypeFromWorkflow(triggerEvent: string): string {
  if (triggerEvent === "form.submit" || triggerEvent.endsWith(".submitted")) return "form_submit";
  if (triggerEvent === "api.workflow.start") return "api";
  if (triggerEvent.endsWith(".import.completed")) return "integration_completed";
  if (triggerEvent.endsWith(".import.failed")) return "integration_failed";
  return "manual";
}

export function AutomationTab() {
  const qc = useQueryClient();
  const empty: RuleState = { name: "", description: "", trigger_type: "manual", workflow_name: "", workflow_def_id: "", source_form_id: "", source_grid_id: "", source_integration_id: "" };
  const [showCreate, setShowCreate] = useState(false);
  const [newRule, setNewRule] = useState<RuleState>(empty);
  const [editId, setEditId] = useState<string | null>(null);
  const [editRule, setEditRule] = useState<RuleState>(empty);

  const { data: demoCtx } = useQuery({ queryKey: ["demo"], queryFn: api.getDemo, staleTime: 60_000 });
  const appId = demoCtx?.app_id ?? "";

  const { data: rules = [] } = useQuery({ queryKey: ["automation-rules"], queryFn: () => api.listAutomationRules() });
  const { data: workflows = [] } = useQuery({
    queryKey: ["dev-workflows", appId],
    queryFn: () => api.listWorkflowDefs(appId),
    enabled: !!appId,
  });
  const { data: forms = [] } = useQuery({ queryKey: ["forms"], queryFn: () => api.listForms() });
  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids", undefined], queryFn: () => api.listGrids() });
  const { data: integrations = [] } = useQuery({ queryKey: ["integrations", undefined], queryFn: () => api.listIntegrations() });
  const { data: executions = [] } = useQuery({
    queryKey: ["executions"], queryFn: api.listExecutions, refetchInterval: 8_000,
  });

  const createRule = useMutation({
    mutationFn: () => api.createAutomationRule(newRule),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["automation-rules"] });
      setNewRule(empty);
      setShowCreate(false);
    },
  });
  const updateRule = useMutation({
    mutationFn: () => api.updateAutomationRule(editId!, editRule),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["automation-rules"] }); setEditId(null); },
  });
  const deleteRule = useMutation({
    mutationFn: (ruleId: string) => api.deleteAutomationRule(ruleId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["automation-rules"] }),
  });
  const toggleEnabled = useMutation({
    mutationFn: ({ ruleId, enabled }: { ruleId: string; enabled: boolean }) =>
      api.updateAutomationRule(ruleId, { enabled }),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["automation-rules"] }),
  });
  const trigger = useMutation({
    mutationFn: (ruleId: string) => api.triggerRule(ruleId),
    onSuccess: () => qc.invalidateQueries({ queryKey: ["executions"] }),
  });
  const { confirm, confirmElement } = useConfirm();

  const startEdit = (r: AutomationRule) => {
    setEditId(r.id);
    setEditRule({ name: r.name, description: r.description, trigger_type: r.trigger_type, workflow_name: r.workflow_name, workflow_def_id: r.workflow_def_id ?? "", source_form_id: r.source_form_id ?? "", source_grid_id: r.source_grid_id ?? "", source_integration_id: r.source_integration_id ?? "" });
  };

  // Find workflow summary for a rule, matching by name (legacy) or id
  const workflowFor = (rule: AutomationRule) =>
    workflows.find(w => w.id === rule.workflow_def_id || w.name === rule.workflow_name);

  return (
    <div>
      <div style={{ marginBottom: 32 }}>
        <SectionHeader
          title="Automation Rules"
          actions={
            <Button leadingIcon={showCreate ? undefined : <Plus size={14} />} onClick={() => setShowCreate((v) => !v)}>
              {showCreate ? "Cancel" : "New rule"}
            </Button>
          }
        />

        {showCreate && (
          <div className="mvx-panel" style={{ padding: 20, marginBottom: 16 }}>
            <RuleForm rule={newRule} onChange={setNewRule} workflows={workflows} forms={forms} grids={grids} integrations={integrations} />
            <Button
              variant="primary"
              style={{ marginTop: 12 }}
              disabled={!newRule.name || !newRule.workflow_name}
              loading={createRule.isPending}
              loadingLabel="Creating…"
              onClick={() => createRule.mutate()}
            >
              Create rule
            </Button>
            {createRule.isError && <p className="mvx-admin-error" style={{ marginTop: 8 }}>{(createRule.error as Error).message}</p>}
          </div>
        )}

        {(rules as AutomationRule[]).length === 0 ? (
          <EmptyState label="No triggers yet." />
        ) : (
          <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
            {(rules as AutomationRule[]).map((rule) => editId === rule.id ? (
              <div key={rule.id} className="mvx-panel" style={{ padding: 16, borderColor: "var(--color-brand-200)" }}>
                <RuleForm rule={editRule} onChange={setEditRule} workflows={workflows} forms={forms} grids={grids} integrations={integrations} />
                <div className="mvx-admin-inline-form" style={{ marginTop: 12 }}>
                  <Button variant="primary" loading={updateRule.isPending} loadingLabel="Saving…" onClick={() => updateRule.mutate()}>
                    Save
                  </Button>
                  <Button onClick={() => setEditId(null)}>Cancel</Button>
                </div>
              </div>
            ) : (
              <div key={rule.id} className="mvx-admin-object" style={{ padding: "12px 16px", display: "flex", alignItems: "center", gap: 16, opacity: rule.enabled ? 1 : 0.7 }}>
                <div style={{ flex: 1 }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <span style={{ fontWeight: 600, fontSize: 13 }}>{rule.name}</span>
                    {!rule.enabled && <StatusBadge>Disabled</StatusBadge>}
                  </div>
                  <div className="mvx-admin-muted" style={{ marginTop: 2, display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
                    <StatusBadge>{rule.trigger_type}</StatusBadge>
                    {rule.source_form_id && (() => { const f = forms.find(x => x.id === rule.source_form_id); return f ? <StatusBadge tone="success">form: {f.label || f.name}</StatusBadge> : null; })()}
                    {rule.source_grid_id && (() => { const g = grids.find(x => x.id === rule.source_grid_id); return g ? <StatusBadge tone="info">grid: {g.name}</StatusBadge> : null; })()}
                    <span>→</span>
                    <span style={{ fontWeight: 500, color: "var(--color-text)" }}>{rule.workflow_name}</span>
                    {(() => {
                      const wf = workflowFor(rule);
                      if (!wf) return <StatusBadge tone="danger">workflow not found</StatusBadge>;
                      if (wf.status === "archived") return <StatusBadge tone="warning">archived</StatusBadge>;
                      if (wf.status === "draft") return <StatusBadge tone="draft">draft</StatusBadge>;
                      if (wf.status === "published") return <StatusBadge tone="live">published</StatusBadge>;
                      return null;
                    })()}
                    {rule.description && <span>· {rule.description}</span>}
                  </div>
                </div>
                <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
                  {rule.trigger_type === "manual" && (
                    <Button
                      variant="primary"
                      size="sm"
                      disabled={trigger.isPending || !rule.enabled}
                      title={!rule.enabled ? "Enable the trigger first" : "Starts a real, live workflow instance right now — same as a business user pressing the button on their dashboard"}
                      onClick={() => confirm({
                        title: "Fire this trigger?",
                        body: `This starts a real, live instance of "${rule.workflow_name}" immediately — it will appear in the assigned approver's Workflow Inbox exactly as if a business user had triggered it themselves. This is not a dry run.`,
                        confirmLabel: "Start it",
                        onConfirm: () => trigger.mutate(rule.id),
                      })}
                    >
                      Trigger
                    </Button>
                  )}
                  <IconButton
                    aria-label={rule.enabled ? `Disable rule ${rule.name}` : `Enable rule ${rule.name}`}
                    title={rule.enabled
                      ? "Disable — stops this trigger from firing (manually or on its event) until re-enabled. Does not delete the rule or affect instances already running."
                      : "Enable — lets this trigger fire again"}
                    disabled={toggleEnabled.isPending}
                    onClick={() => {
                      if (!rule.enabled) {
                        toggleEnabled.mutate({ ruleId: rule.id, enabled: true });
                        return;
                      }
                      confirm({
                        title: "Disable this trigger?",
                        body: `"${rule.name}" will stop firing — the "Trigger" button and any event it listens for (e.g. form submit) will no longer start "${rule.workflow_name}". The rule itself, the workflow, and any already-running instances are unaffected. You can re-enable it anytime.`,
                        confirmLabel: "Disable",
                        onConfirm: () => toggleEnabled.mutate({ ruleId: rule.id, enabled: false }),
                      });
                    }}
                  >
                    {rule.enabled ? <Pause size={14} /> : <Play size={14} />}
                  </IconButton>
                  <IconButton aria-label={`Edit rule ${rule.name}`} title="Edit" onClick={() => startEdit(rule)}>
                    <Pencil size={14} />
                  </IconButton>
                  <IconButton
                    aria-label={`Delete rule ${rule.name}`}
                    title="Delete"
                    danger
                    onClick={() => confirm({ title: "Delete rule?", body: `This removes "${rule.name}" and stops any future triggers.`, confirmLabel: "Delete rule", onConfirm: () => deleteRule.mutate(rule.id) })}
                  >
                    <Trash2 size={14} />
                  </IconButton>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      <div>
        <SectionHeader title="Execution Log" />
        {(executions as Execution[]).length === 0 ? (
          <EmptyState label="No executions yet. Trigger a rule above." />
        ) : (
          <div className="mvx-table-wrap">
            <table className="mvx-table mvx-table--compact">
              <thead>
                <tr>
                  <th>ID</th><th>Status</th><th>Instance</th><th>Started</th>
                </tr>
              </thead>
              <tbody>
                {(executions as Execution[]).map((e) => (
                  <tr key={e.id}>
                    <td className="mvx-admin-mono mvx-admin-muted">{e.id.slice(0, 8)}</td>
                    <td>
                      <StatusBadge tone={EXEC_STATUS_TONE[e.status] ?? "neutral"}>{e.status}</StatusBadge>
                    </td>
                    <td className="mvx-admin-mono mvx-admin-muted">
                      {e.instance_id ? e.instance_id.slice(0, 8) : "—"}
                    </td>
                    <td className="mvx-admin-muted">{new Date(e.started_at).toLocaleString()}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
      {confirmElement}
    </div>
  );
}

function RuleForm({
  rule, onChange, workflows, forms, grids, integrations }: {
  rule: RuleState;
  onChange: (r: RuleState) => void;
  workflows: WorkflowDefSummary[];
  forms: FormDef[];
  grids: GridDef[];
  integrations: IntegrationDef[];
}) {
  const published = workflows.filter(w => w.status === "published");
  const others = workflows.filter(w => w.status !== "published");
  const needsForm = rule.trigger_type === "form_submit" || rule.trigger_type === "form_approval";
  const needsGrid = rule.trigger_type === "grid_change";
  const needsIntegration = rule.trigger_type === "integration_completed" || rule.trigger_type === "integration_failed";

  const handleWorkflowSelect = (e: React.ChangeEvent<HTMLSelectElement>) => {
    const wf = workflows.find(w => w.id === e.target.value);
    onChange({
      ...rule,
      workflow_def_id: e.target.value,
      workflow_name: wf?.name ?? "",
      trigger_type: wf ? triggerTypeFromWorkflow(wf.trigger_event) : rule.trigger_type,
      source_form_id: "",
      source_grid_id: "",
      source_integration_id: "",
    });
  };
  const workflowLocked = !!rule.workflow_def_id;

  return (
    <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
      <Field label="Name">
        <TextInput value={rule.name} placeholder="rule_name"
          onChange={(e) => onChange({ ...rule, name: e.target.value })} />
      </Field>
      <Field
        label="Workflow"
        description={workflows.length === 0 ? "No workflows yet — create one in the Workflows tab." : undefined}
      >
        {workflows.length > 0 ? (
          <Select value={rule.workflow_def_id || rule.workflow_name} onChange={handleWorkflowSelect}>
            <option value="">— select workflow —</option>
            {published.length > 0 && (
              <optgroup label="Published">
                {published.map(w => <option key={w.id} value={w.id}>{w.name} ({w.step_count} steps)</option>)}
              </optgroup>
            )}
            {others.length > 0 && (
              <optgroup label="Draft / Other">
                {others.map(w => <option key={w.id} value={w.id}>{w.name} [{w.status}]</option>)}
              </optgroup>
            )}
          </Select>
        ) : (
          <TextInput value={rule.workflow_name} placeholder="Workflow name"
            onChange={(e) => onChange({ ...rule, workflow_name: e.target.value })} />
        )}
      </Field>
      <Field
        label="Trigger type"
        description={workflowLocked ? "Set by the workflow's trigger event." : undefined}
      >
        <Select value={rule.trigger_type}
          onChange={(e) => onChange({ ...rule, trigger_type: e.target.value, source_form_id: "", source_grid_id: "", source_integration_id: "" })}
          disabled={workflowLocked}
          title={workflowLocked ? "Derived from the workflow's trigger event" : undefined}>
          <option value="manual">manual — trigger manually</option>
          <option value="form_submit">form_submit — on record creation</option>
          <option value="form_approval">form_approval — on record approved / closed</option>
          <option value="grid_change">grid_change — on cell writeback</option>
          <option value="integration_completed">integration_completed — an integration run finished</option>
          <option value="integration_failed">integration_failed — an integration run failed</option>
          <option value="api">api — external HTTP trigger</option>
        </Select>
      </Field>
      {needsForm && (
        <Field label="Source form" description="optional — leave blank for any form">
          <Select value={rule.source_form_id} onChange={(e) => onChange({ ...rule, source_form_id: e.target.value })}>
            <option value="">— any form —</option>
            {forms.map(f => <option key={f.id} value={f.id}>{f.label || f.name}</option>)}
          </Select>
        </Field>
      )}
      {needsGrid && (
        <Field label="Source grid" description="optional — leave blank for any grid">
          <Select value={rule.source_grid_id} onChange={(e) => onChange({ ...rule, source_grid_id: e.target.value })}>
            <option value="">— any grid —</option>
            {grids.map(g => <option key={g.id} value={g.id}>{g.name}</option>)}
          </Select>
        </Field>
      )}
      {needsIntegration && (
        <Field label="Source integration" description="optional — leave blank for any integration">
          <Select value={rule.source_integration_id} onChange={(e) => onChange({ ...rule, source_integration_id: e.target.value })}>
            <option value="">— any integration —</option>
            {integrations.map(i => <option key={i.id} value={i.id}>{i.name}</option>)}
          </Select>
        </Field>
      )}
      <Field label="Description" style={{ gridColumn: needsForm || needsGrid || needsIntegration ? "1 / -1" : undefined }}>
        <TextInput value={rule.description} placeholder="What this rule does"
          onChange={(e) => onChange({ ...rule, description: e.target.value })} />
      </Field>
    </div>
  );
}
