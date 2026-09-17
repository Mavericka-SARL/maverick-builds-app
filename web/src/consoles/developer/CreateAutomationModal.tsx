import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import type { WorkflowDef, WorkflowDefSummary } from "../../api/client";
import { Modal, Field } from "./WorkflowShared";
import { inputStyle, btnPrimary, btnSecondary } from "./workflowConstants";

// ── CreateAutomationModal ─────────────────────────────────────────────────────

export function CreateAutomationModal({ workflow, onClose }: { workflow: WorkflowDefSummary | WorkflowDef; onClose: () => void }) {
  const qc = useQueryClient();
  const [name, setName] = useState(`${workflow.name} — Trigger`);
  const [description, setDescription] = useState("");

  const triggerType =
    workflow.trigger_event === "form.submit" ? "form_submit"
      : workflow.trigger_event === "api.workflow.start" ? "api"
      : "manual";

  const createMutation = useMutation({
    mutationFn: () =>
      api.createAutomationRule({
        name,
        description,
        trigger_type: triggerType,
        workflow_name: workflow.name,
      }),
    onSuccess: () => {
      qc.invalidateQueries({ queryKey: ["automation-rules"] });
      onClose();
    },
  });

  return (
    <Modal title="Create Trigger" onClose={onClose}>
      <div style={{ display: "flex", flexDirection: "column", gap: 14 }}>
        <div style={{ background: "var(--color-diagram-calc-bg)", border: "1px solid var(--color-success-border)", borderRadius: 8, padding: "10px 12px", fontSize: 13, color: "var(--color-success-chip-text)", display: "flex", justifyContent: "space-between", alignItems: "center" }}>
          <span>Workflow: <strong>{workflow.name}</strong></span>
          <span style={{ background: "var(--color-success-chip-bg)", color: "var(--color-live)", borderRadius: 4, padding: "1px 7px", fontSize: 11, fontWeight: 600 }}>{triggerType}</span>
        </div>
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
            disabled={!name.trim() || createMutation.isPending}
            style={btnPrimary}>
            {createMutation.isPending ? "Creating…" : "Create Trigger"}
          </button>
        </div>
      </div>
    </Modal>
  );
}
