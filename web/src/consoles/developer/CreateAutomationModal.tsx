import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { WorkflowDef, WorkflowDefSummary } from "../../api/client";
import { Modal, Field } from "./WorkflowShared";
import { inputStyle, btnPrimary, btnSecondary, ruleTriggerFromWorkflow, useTriggerEvents } from "./workflowConstants";

// ── CreateAutomationModal ─────────────────────────────────────────────────────

// revisionId is the Build working revision the workflow was opened in: the
// rule is created there, not in the default model's live revision.
export function CreateAutomationModal({ workflow, revisionId, onClose }: { workflow: WorkflowDefSummary | WorkflowDef; revisionId?: string; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(`${workflow.name} — Trigger`);
  const [description, setDescription] = useState("");

  // The same mapping Build › Triggers uses: a per-form or per-integration
  // event used to fall through to "manual" here, so the rule never fired —
  // and, typed but unscoped, it would fire for every form or import. The
  // source comes from the working revision's trigger-event catalog.
  const { data: catalog, isLoading: catalogLoading } = useTriggerEvents(workflow.application_id, revisionId);
  const trigger = ruleTriggerFromWorkflow(workflow.trigger_event, catalog);
  const triggerType = trigger.trigger_type;
  const sourceName = catalog?.find(c => c.key === workflow.trigger_event)?.source_name;

  const createMutation = useMutation({
    mutationFn: () =>
      api.createAutomationRule({
        name,
        description,
        trigger_type: triggerType,
        workflow_name: workflow.name,
        workflow_def_id: workflow.id,
        ...(trigger.source_form_id ? { source_form_id: trigger.source_form_id } : {}),
        ...(trigger.source_integration_id ? { source_integration_id: trigger.source_integration_id } : {}),
      }, revisionId),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["automation-rules"] });
      // The workflow's Usage panel lists the rules that start it.
      qc.invalidateQueries({ queryKey: ["dev-workflow-usage", workflow.id] });
      onClose();
    },
  });

  return (
    <Modal title="Create Trigger" onClose={onClose}>
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div style={{ background: "var(--color-diagram-calc-bg)", border: "1px solid var(--color-success-border)", borderRadius: 8, padding: "10px 12px", fontSize: 13, color: "var(--color-success-chip-text)", display: "flex", justifyContent: "space-between", alignItems: "center" }}>
          <span>Workflow: <strong>{workflow.name}</strong></span>
          <span style={{ background: "var(--color-success-chip-bg)", color: "var(--color-live)", borderRadius: 4, padding: "1px 7px", fontSize: 11, fontWeight: 600 }}>
            {triggerType}{sourceName && !trigger.unresolved ? ` · ${sourceName}` : ""}
          </span>
        </div>
        {trigger.unresolved && !catalogLoading && (
          <div role="alert" style={{ color: "var(--color-danger)", fontSize: 13 }}>
            This workflow starts on “{workflow.trigger_event}”, which names no form or integration of this revision.
            Choose its trigger event again in the workflow's properties, or create the trigger in Build › Triggers.
          </div>
        )}
        <Field label="Name">
          <input autoFocus value={name} onChange={e => setName(e.target.value)} style={inputStyle} />
        </Field>
        <Field label="Description">
          <input value={description} onChange={e => setDescription(e.target.value)} placeholder="Optional" style={inputStyle} />
        </Field>
        {createMutation.isError && (
          <div style={{ color: "var(--color-danger)", fontSize: 13 }}>{String(createMutation.error)}</div>
        )}
        <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
          <button onClick={onClose} style={btnSecondary}>Cancel</button>
          <button
            onClick={() => createMutation.mutate()}
            disabled={!name.trim() || createMutation.isPending || trigger.unresolved}
            style={btnPrimary}>
            {createMutation.isPending ? "Creating…" : "Create Trigger"}
          </button>
        </div>
      </div>
    </Modal>
  );
}
