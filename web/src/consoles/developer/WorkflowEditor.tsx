import { useState, useCallback } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { StatusBadge as UiStatusBadge, useConfirm } from "../../ui";
import { api } from "../../api/client";
import type { WorkflowDef, WorkflowStepDef, StepType, WorkflowDefUsage, WorkflowSubjectType } from "../../api/client";
import { StepTypeIcon, StatusBadge, Modal, Field, IconBtn } from "./WorkflowShared";
import { STEP_TYPE_LABELS, inputStyle, btnPrimary, btnSecondary } from "./workflowConstants";
import { ProcessCanvas } from "./WorkflowCanvas";
import { StepPropertiesPanel } from "./StepPropertiesPanel";
import { WorkflowPropertiesPanel } from "./WorkflowPropertiesPanel";

// ── WorkflowDef normalization ─────────────────────────────────────────────────

const STEP_TYPE_INT_MAP: Record<string, StepType> = {
  "1": "task", "2": "approval", "3": "notification", "4": "condition", "5": "join",
};
const VALID_STEP_TYPES = new Set<string>(["task", "approval", "notification", "condition", "join"]);

function normalizeStepType(type: unknown): StepType {
  const s = String(type ?? "");
  if (VALID_STEP_TYPES.has(s)) return s as StepType;
  return STEP_TYPE_INT_MAP[s] ?? "task";
}

function normalizeWorkflowDef(def: WorkflowDef): WorkflowDef {
  return {
    ...def,
    subject_type: (def.subject_type ?? "") as WorkflowSubjectType,
    subject_config: (def.subject_config ?? {}) as Record<string, string>,
    steps: (def.steps ?? []).map(s => ({ ...s, type: normalizeStepType(s.type) })),
  };
}

// ── Helpers ──────────────────────────────────────────────────────────────────

function genId(): string {
  return `step-${Math.random().toString(36).slice(2, 9)}`;
}

// ── WorkflowEditor ───────────────────────────────────────────────────────────

interface EditorProps {
  defId: string;
  applicationId: string;
  revisionId?: string;
  onBack: () => void;
}

export function WorkflowEditor({ defId, applicationId, revisionId, onBack }: EditorProps) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const [selectedStepId, setSelectedStepId] = useState<string | null>(null);
  const [propertiesSection, setPropertiesSection] = useState<"properties" | "context" | "usage" | "instances">("properties");

  // Staged local state
  const [draft, setDraft] = useState<WorkflowDef | null>(null);
  const [isDirty, setIsDirty] = useState(false);
  const [saveState, setSaveState] = useState<"idle" | "saving" | "saved" | "error">("idle");
  const [validationErrors, setValidationErrors] = useState<string[]>([]);
  const [showValidation, setShowValidation] = useState(false);
  const [showTestRun, setShowTestRun] = useState(false);
  const [testRunContext, setTestRunContext] = useState<Record<string, string>>({});
  const [testRunResult, setTestRunResult] = useState<{ instance_id: string; status: string; test_run?: boolean; current_step_id?: string; current_step_status?: string } | null>(null);

  const { data: def, isLoading } = useQuery({
    queryKey: ["dev-workflow", defId],
    queryFn: () => api.getWorkflowDef(defId),
    enabled: !!defId,
    select: normalizeWorkflowDef,
  });

  const { data: usage = [] } = useQuery<WorkflowDefUsage[]>({
    queryKey: ["dev-workflow-usage", defId],
    queryFn: () => api.getWorkflowDefUsage(defId),
    enabled: !!defId,
  });

  // Initialise draft from server data
  const effective = draft ?? def;

  const setEffective = useCallback((updater: (prev: WorkflowDef) => WorkflowDef) => {
    setDraft(prev => {
      const base = prev ?? def!;
      const next = updater(base);
      setIsDirty(true);
      setSaveState("idle");
      return next;
    });
  }, [def]);

  const saveMutation = useMutation({
    mutationFn: (d: WorkflowDef) =>
      api.updateWorkflowDef(d.id, {
        name: d.name,
        description: d.description,
        trigger_event: d.trigger_event,
        subject_type: d.subject_type,
        subject_config: d.subject_config,
        steps: d.steps,
        context_schema: d.context_schema,
        single_active_instance: d.single_active_instance,
      }),
    onMutate: () => setSaveState("saving"),
    onSuccess: (saved) => {
      qc.setQueryData(["dev-workflow", defId], saved);
      qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] });
      setDraft(null);
      setIsDirty(false);
      setSaveState("saved");
      setTimeout(() => setSaveState("idle"), 2000);
    },
    onError: () => setSaveState("error"),
  });

  const publishMutation = useMutation({
    mutationFn: () => api.publishWorkflowDef(defId),
    onSuccess: (saved) => {
      qc.setQueryData(["dev-workflow", defId], saved);
      qc.invalidateQueries({ queryKey: ["dev-workflows", applicationId] });
      setDraft(null);
      setIsDirty(false);
    },
  });

  const validateMutation = useMutation({
    // Validates the DRAFT as it is on screen — nothing is saved. (It used to
    // persist a steps-only PATCH first, which both saved behind the user's
    // back and wiped the workflow's name, trigger and subject.)
    mutationFn: async () =>
      api.validateWorkflowDef(defId, isDirty && effective
        ? { name: effective.name, steps: effective.steps, context_schema: effective.context_schema }
        : undefined),
    onSuccess: (result) => {
      setValidationErrors(result.errors ?? []);
      setShowValidation(true);
    },
  });

  const testRunMutation = useMutation({
    mutationFn: () => api.testRunWorkflowDef(defId, testRunContext),
    onSuccess: (res) => setTestRunResult(res),
  });

  const cancelChanges = () => {
    setDraft(null);
    setIsDirty(false);
    setSaveState("idle");
  };

  const handleBack = () => {
    if (!isDirty) { onBack(); return; }
    confirm({
      title: "Discard unsaved changes?",
      body: "Your edits to this workflow have not been saved.",
      confirmLabel: "Discard changes",
      onConfirm: onBack,
    });
  };

  if (isLoading || !effective) {
    return <div style={{ padding: 32, color: "var(--color-text-quiet)" }}>Loading…</div>;
  }

  const selectedStep = effective.steps.find(s => s.id === selectedStepId) ?? null;

  const updateStep = (id: string, patch: Partial<WorkflowStepDef>) => {
    setEffective(prev => ({
      ...prev,
      steps: prev.steps.map(s => s.id === id ? { ...s, ...patch } : s),
    }));
  };

  const addStep = (type: StepType) => {
    const newStep: WorkflowStepDef = {
      id: genId(), name: `New ${STEP_TYPE_LABELS[type]}`, type, assignee_roles: [], routes: {},
      // "requester" always resolves to exactly one real user with no extra
      // setup — a safer default than "role", which silently notifies nobody
      // until a role is also picked.
      ...(type === "notification" ? { notification: { recipient_type: "requester" as const, subject: "", message: "" } } : {}),
    };
    setEffective(prev => {
      const steps = [...prev.steps];
      if (selectedStepId) {
        const idx = steps.findIndex(s => s.id === selectedStepId);
        if (idx >= 0) {
          steps.splice(idx + 1, 0, newStep);
          return { ...prev, steps };
        }
      }
      return { ...prev, steps: [...steps, newStep] };
    });
    setSelectedStepId(newStep.id);
  };

  const removeStep = (id: string) => {
    setEffective(prev => ({ ...prev, steps: prev.steps.filter(s => s.id !== id) }));
    if (selectedStepId === id) setSelectedStepId(null);
  };

  const moveStep = (id: string, dir: -1 | 1) => {
    setEffective(prev => {
      const idx = prev.steps.findIndex(s => s.id === id);
      if (idx < 0) return prev;
      const newIdx = idx + dir;
      if (newIdx < 0 || newIdx >= prev.steps.length) return prev;
      const steps = [...prev.steps];
      [steps[idx], steps[newIdx]] = [steps[newIdx], steps[idx]];
      return { ...prev, steps };
    });
  };

  const dupStep = (id: string) => {
    setEffective(prev => {
      const idx = prev.steps.findIndex(s => s.id === id);
      if (idx < 0) return prev;
      const copy = { ...prev.steps[idx], id: genId(), name: prev.steps[idx].name + " (copy)" };
      const steps = [...prev.steps];
      steps.splice(idx + 1, 0, copy);
      return { ...prev, steps };
    });
  };

  const saveLabel = saveState === "saving" ? "Saving…" : saveState === "saved" ? "Saved ✓" : saveState === "error" ? "Save failed" : "Save";

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100vh", overflow: "hidden" }}>

      {/* Top toolbar */}
      <div style={{ display: "flex", alignItems: "center", gap: 12, padding: "0 20px", height: 52, borderBottom: "1px solid var(--color-border)", background: "var(--color-surface)", flexShrink: 0 }}>
        <button onClick={handleBack} style={{ background: "none", border: "none", cursor: "pointer", color: "var(--color-text-quiet)", fontSize: 13, padding: "4px 6px" }}>
          ← Workflows
        </button>
        <div style={{ width: 1, height: 20, background: "var(--color-border)" }} />
        {/* The name is edited in Properties; a second input here was the
            same field twice (constructor audit, 2026-09-10). */}
        <span style={{ fontSize: 15, fontWeight: 700, color: "var(--color-text)", minWidth: 200, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis" }} title={effective.name}>
          {effective.name || "Untitled workflow"}
        </span>
        <StatusBadge status={effective.status} />
        {/* Trigger connections — always visible here (not just on a Usage
            tab that disappears the moment a step is selected), since
            "what starts this workflow" is exactly what's easy to lose
            track of once you're editing an individual step. */}
        <button
          onClick={() => { setSelectedStepId(null); setPropertiesSection("usage"); }}
          title={usage.length === 0 ? "No automation rule starts this workflow yet — click to create one" : "View/edit the rules that start this workflow"}
          style={{
            display: "flex", alignItems: "center", gap: 6, border: "none", cursor: "pointer",
            background: "transparent", padding: "4px 6px", borderRadius: 6,
          }}
        >
          {usage.length === 0 ? (
            <UiStatusBadge tone="warning">⚡ No rule yet</UiStatusBadge>
          ) : (
            usage.map(u => (
              <UiStatusBadge key={u.rule_id} tone={u.enabled ? "info" : "warning"}>
                ⚡ {u.rule_name}{!u.enabled && " (disabled)"}
              </UiStatusBadge>
            ))
          )}
        </button>
        {isDirty && <span style={{ fontSize: 12, color: "var(--color-warning-accent)" }}>Unsaved changes</span>}
        {isDirty && effective.status === "published" && (
          <span style={{ fontSize: 12, color: "var(--color-text-quiet)" }} title="Running instances keep the definition they started with; only instances started after you save use the new one. Changes across revisions belong to the revision.">
            Published — saving applies to new starts; running instances keep the definition they started with
          </span>
        )}
        {(publishMutation.isError || saveMutation.isError) && (
          <span role="alert" style={{ fontSize: 12, color: "var(--color-danger)" }}>
            {String((publishMutation.error ?? saveMutation.error) as Error).replace(/^Error:\s*/, "")}
          </span>
        )}
        <div style={{ flex: 1 }} />
        <button
          onClick={() => validateMutation.mutate()}
          disabled={validateMutation.isPending}
          style={btnSecondary}>
          Validate
        </button>
        <button
          onClick={() => setShowTestRun(true)}
          disabled={isDirty}
          title={isDirty ? "Save first — a test run uses the saved definition" : "Start a test run"}
          style={{ ...btnSecondary, opacity: isDirty ? 0.5 : 1 }}>
          Test Run
        </button>
        {isDirty && (
          <button onClick={cancelChanges} style={btnSecondary}>Cancel</button>
        )}
        <button
          onClick={() => effective && saveMutation.mutate(effective)}
          disabled={saveMutation.isPending || !isDirty}
          style={{ ...btnPrimary, background: saveState === "saved" ? "var(--color-success)" : saveState === "error" ? "var(--color-danger)" : "var(--color-text)" }}>
          {saveLabel}
        </button>
        {effective.status !== "published" && (
          <button
            onClick={() => publishMutation.mutate()}
            disabled={publishMutation.isPending || isDirty}
            title={isDirty ? "Save first" : "Publish"}
            style={{ ...btnPrimary, background: "var(--color-success)", opacity: isDirty ? 0.5 : 1 }}>
            Publish
          </button>
        )}
      </div>

      {/* Main body: left step list + center canvas + right properties */}
      <div style={{ display: "flex", flex: 1, overflow: "hidden" }}>

        {/* Left: step list */}
        <div style={{ width: 240, borderRight: "1px solid var(--color-border)", display: "flex", flexDirection: "column", overflow: "hidden", background: "var(--color-grid-row-alt-bg)" }}>
          <div style={{ padding: "12px 14px 8px", fontWeight: 600, fontSize: 12, color: "var(--color-text-quiet)", textTransform: "uppercase", letterSpacing: "0.05em" }}>
            Steps
          </div>
          <div style={{ flex: 1, overflowY: "auto" }}>
            {effective.steps.length === 0 && (
              <div style={{ padding: "20px 14px", fontSize: 13, color: "var(--color-disabled)" }}>No steps yet. Add one below.</div>
            )}
            {effective.steps.map((step, idx) => (
              <StepListItem
                key={step.id}
                step={step}
                index={idx}
                total={effective.steps.length}
                selected={selectedStepId === step.id}
                onSelect={() => setSelectedStepId(step.id)}
                onMoveUp={() => moveStep(step.id, -1)}
                onMoveDown={() => moveStep(step.id, 1)}
                onDuplicate={() => dupStep(step.id)}
                onDelete={() => removeStep(step.id)}
              />
            ))}
          </div>
          {/* Add step buttons */}
          <div style={{ padding: "10px 12px", borderTop: "1px solid var(--color-border)" }}>
            <div style={{ fontSize: 11, color: "var(--color-disabled)", marginBottom: 6, fontWeight: 600, textTransform: "uppercase" }}>Add step</div>
            <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 6 }}>
              {(["task", "approval", "condition", "notification", "join"] as StepType[]).map(type => (
                <button key={type} onClick={() => addStep(type)} style={{ padding: "6px 4px", border: "1px solid var(--color-border)", borderRadius: 6, background: "var(--color-surface)", fontSize: 12, cursor: "pointer", color: "var(--color-text-strong)" }}>
                  <StepTypeIcon type={type} size={13} /> {STEP_TYPE_LABELS[type]}
                </button>
              ))}
            </div>
          </div>
        </div>

        {/* Center: process canvas */}
        <div style={{ flex: 1, overflowY: "auto", background: "var(--color-surface-faint)" }}>
          <ProcessCanvas
            steps={effective.steps}
            selectedId={selectedStepId}
            onSelect={setSelectedStepId}
          />
        </div>

        {/* Right: properties panel */}
        <div style={{ width: 300, borderLeft: "1px solid var(--color-border)", overflowY: "auto", background: "var(--color-surface)" }}>
          {selectedStep ? (
            <StepPropertiesPanel
              step={selectedStep}
              allSteps={effective.steps}
              applicationId={applicationId}
              onChange={patch => updateStep(selectedStep.id, patch)}
              onDelete={() => removeStep(selectedStep.id)}
              onClose={() => setSelectedStepId(null)}
            />
          ) : (
            <WorkflowPropertiesPanel
              workflow={effective}
              usage={usage}
              onChange={patch => setEffective(prev => ({ ...prev, ...patch }))}
              applicationId={applicationId}
              revisionId={revisionId}
              activeSection={propertiesSection}
              onSectionChange={setPropertiesSection}
            />
          )}
        </div>
      </div>

      {confirmElement}
      {/* Validation panel */}
      {showValidation && (
        <Modal title="Validation Results" onClose={() => setShowValidation(false)}>
          {validationErrors.length === 0 ? (
            <div style={{ color: "var(--color-success)", fontWeight: 600, fontSize: 14 }}>✓ Workflow is valid</div>
          ) : (
            <ul style={{ margin: 0, paddingLeft: 18, fontSize: 13, color: "var(--color-danger)" }}>
              {validationErrors.map((e, i) => <li key={i}>{e}</li>)}
            </ul>
          )}
          <div style={{ marginTop: 16, display: "flex", justifyContent: "flex-end" }}>
            <button onClick={() => setShowValidation(false)} style={btnPrimary}>Close</button>
          </div>
        </Modal>
      )}

      {/* Test run modal */}
      {showTestRun && (
        <Modal title="Test Run" onClose={() => { setShowTestRun(false); setTestRunResult(null); }}>
          {testRunResult ? (
            <div>
              <div style={{ color: "var(--color-success)", fontWeight: 600, fontSize: 14, marginBottom: 8 }}>✓ Test run started</div>
              <p style={{ fontSize: 12, color: "var(--color-text-quiet)", margin: "0 0 10px" }}>
                Runs the real routing, but sends no notifications and writes no data.
              </p>
              <div style={{ fontSize: 13, color: "var(--color-text-strong)" }}>Instance ID: <code style={{ background: "var(--color-surface-muted)", padding: "2px 6px", borderRadius: 4 }}>{testRunResult.instance_id}</code></div>
              <div style={{ fontSize: 13, color: "var(--color-text-strong)", marginTop: 6 }}>Status: {testRunResult.status}</div>
              {testRunResult.current_step_id && (
                <div style={{ fontSize: 13, color: "var(--color-text-strong)", marginTop: 6 }}>
                  Stopped at step: <code style={{ background: "var(--color-surface-muted)", padding: "2px 6px", borderRadius: 4 }}>{testRunResult.current_step_id}</code>
                </div>
              )}
              <div style={{ marginTop: 16, display: "flex", justifyContent: "flex-end" }}>
                <button onClick={() => { setShowTestRun(false); setTestRunResult(null); }} style={btnPrimary}>Close</button>
              </div>
            </div>
          ) : (
            <div>
              <p style={{ fontSize: 13, color: "var(--color-text-quiet)", margin: "0 0 14px" }}>
                Provide context variables for the test run. The workflow will start in test mode.
              </p>
              {effective.context_schema.map(cv => (
                <Field key={cv.key} label={cv.label || cv.key}>
                  <input
                    value={testRunContext[cv.key] ?? ""}
                    onChange={e => setTestRunContext(p => ({ ...p, [cv.key]: e.target.value }))}
                    placeholder={cv.default_value ?? ""}
                    style={inputStyle}
                  />
                </Field>
              ))}
              {testRunMutation.isError && (
                <div style={{ color: "var(--color-danger)", fontSize: 13, margin: "8px 0" }}>{String(testRunMutation.error)}</div>
              )}
              <div style={{ display: "flex", gap: 8, justifyContent: "flex-end", marginTop: 16 }}>
                <button onClick={() => setShowTestRun(false)} style={btnSecondary}>Cancel</button>
                <button onClick={() => testRunMutation.mutate()} disabled={testRunMutation.isPending} style={btnPrimary}>
                  {testRunMutation.isPending ? "Starting…" : "Start Test Run"}
                </button>
              </div>
            </div>
          )}
        </Modal>
      )}
    </div>
  );
}

// ── StepListItem ─────────────────────────────────────────────────────────────

function StepListItem({ step, index, total, selected, onSelect, onMoveUp, onMoveDown, onDuplicate, onDelete }: {
  step: WorkflowStepDef;
  index: number;
  total: number;
  selected: boolean;
  onSelect: () => void;
  onMoveUp: () => void;
  onMoveDown: () => void;
  onDuplicate: () => void;
  onDelete: () => void;
}) {
  const [hover, setHover] = useState(false);

  return (
    <div
      onClick={onSelect}
      onMouseEnter={() => setHover(true)}
      onMouseLeave={() => setHover(false)}
      style={{
        display: "flex", alignItems: "center", gap: 8, padding: "8px 14px",
        cursor: "pointer", background: selected ? "var(--color-selected-bg)" : hover ? "var(--color-surface-muted)" : "transparent",
        borderLeft: selected ? "3px solid var(--color-selected)" : "3px solid transparent",
      }}>
      <span style={{ fontSize: 11, color: "var(--color-disabled)", width: 16, flexShrink: 0, textAlign: "center" }}>{index + 1}</span>
      <StepTypeIcon type={step.type} size={14} />
      <div style={{ flex: 1, minWidth: 0 }}>
        <div style={{ fontSize: 13, fontWeight: 500, color: "var(--color-text)", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{step.name}</div>
        <div style={{ fontSize: 11, color: "var(--color-disabled)" }}>{STEP_TYPE_LABELS[step.type]}</div>
      </div>
      {hover && (
        <div style={{ display: "flex", gap: 2, flexShrink: 0 }} onClick={e => e.stopPropagation()}>
          <IconBtn title="Move up" disabled={index === 0} onClick={onMoveUp}>↑</IconBtn>
          <IconBtn title="Move down" disabled={index === total - 1} onClick={onMoveDown}>↓</IconBtn>
          <IconBtn title="Duplicate" onClick={onDuplicate}>⊕</IconBtn>
          <IconBtn title="Delete" onClick={onDelete} danger>✕</IconBtn>
        </div>
      )}
    </div>
  );
}
