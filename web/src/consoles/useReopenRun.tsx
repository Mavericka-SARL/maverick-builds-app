import { useRef } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import { Field, TextInput, useConfirm } from "../ui";

/**
 * Taking back a finished run's approval (POST /api/workflow/instances/{id}/reopen),
 * the same dialog in a business admin's History and a developer's Instances.
 * The reason is required: what the run approved was agreed, so the audit log
 * and the people involved are told why.
 */
export function useReopenRun() {
  const qc = useQueryClient();
  // An uncontrolled field, read when the dialog confirms.
  const reason = useRef("");
  const reopen = useMutation({
    mutationFn: ({ instanceId, reason }: { instanceId: string; reason: string }) => api.reopenWorkflowInstance(instanceId, reason),
    onSuccess: () => {
      for (const key of ["executions", "workflow-instances", "tasks", "wf-history"]) {
        qc.invalidateQueries({ queryKey: [key] });
      }
    },
  });
  const { confirm, confirmElement } = useConfirm();
  const askReopen = (instanceId: string, label: string) => {
    reason.current = "";
    confirm({
      title: "Reopen this run?",
      body: (
        <>
          <p style={{ margin: "0 0 10px" }}>
            {label} stops standing as approved, and whatever it locked can be changed again. Its steps, decisions and
            posted values stay as they are. The people involved are told, and the reason goes into the audit log.
          </p>
          <Field label="Reason (required)">
            <TextInput maxLength={500} placeholder="e.g. March was locked with a wrong figure" aria-label="Reason"
              onChange={(ev) => { reason.current = ev.target.value; }} />
          </Field>
        </>
      ),
      confirmLabel: "Reopen run",
      onConfirm: () => reopen.mutate({ instanceId, reason: reason.current }),
    });
  };
  return {
    askReopen,
    pending: reopen.isPending,
    error: reopen.isError ? (reopen.error as Error).message : null,
    confirmElement,
  };
}
