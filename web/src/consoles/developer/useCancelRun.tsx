import { useRef } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import { Field, TextInput, useConfirm } from "../../ui";

/**
 * Stopping a running workflow run (POST /api/workflow/instances/{id}/cancel),
 * the same dialog wherever a developer sees one: Triggers' execution log and
 * a workflow's Instances. The reason is optional and goes into the
 * notification its assignees and starter get.
 */
export function useCancelRun() {
  const qc = useQueryClient();
  // An uncontrolled field, read when the dialog confirms.
  const reason = useRef("");
  const cancel = useMutation({
    mutationFn: ({ instanceId, reason }: { instanceId: string; reason: string }) => api.cancelWorkflowInstance(instanceId, reason),
    onSuccess: () => {
      for (const key of ["executions", "workflow-instances", "tasks", "wf-history"]) {
        qc.invalidateQueries({ queryKey: [key] });
      }
    },
  });
  const { confirm, confirmElement } = useConfirm();
  const askCancel = (instanceId: string, label: string) => {
    reason.current = "";
    confirm({
      title: "Cancel this run?",
      body: (
        <>
          <p style={{ margin: "0 0 10px" }}>
            Run {label} stops: its open steps leave everyone's inbox, and the people they were assigned to
            and the person who started it are notified. Steps already decided keep their effect.
          </p>
          <Field label="Reason (optional, shown in the notification)">
            <TextInput maxLength={500} placeholder="e.g. started by mistake" aria-label="Reason"
              onChange={(ev) => { reason.current = ev.target.value; }} />
          </Field>
        </>
      ),
      confirmLabel: "Cancel run",
      onConfirm: () => cancel.mutate({ instanceId, reason: reason.current }),
    });
  };
  return {
    askCancel,
    pending: cancel.isPending,
    error: cancel.isError ? (cancel.error as Error).message : null,
    confirmElement,
  };
}
