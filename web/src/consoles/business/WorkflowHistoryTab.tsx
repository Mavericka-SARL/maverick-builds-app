import { useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type HistoryInstance, type HistoryStep } from "../../api/client";
import { LoadingState, EmptyState, StatusBadge, type DesignTone } from "../../ui";
import { StepTypeIcon } from "./WorkflowInboxTab";
import { TaskContextSummary } from "../TaskContextSummary";

const WF_STATUS_TONE: Record<string, DesignTone> = {
  running: "info",
  completed: "success",
  cancelled: "neutral",
  failed: "danger",
};
const WF_DECISION_TONE: Record<string, DesignTone> = { approve: "success", reject: "danger" };
const WF_STEP_STATUS_TONE: Record<string, DesignTone> = {
  completed: "success",
  rejected: "danger",
  in_progress: "warning",
};

export function WorkflowMyHistory({ focusInstanceId }: { focusInstanceId?: string }) {
  const { data: instances = [], isLoading } = useQuery({
    queryKey: ["my-wf-history"],
    queryFn: api.getMyWorkflowHistory,
    refetchInterval: 10_000,
  });

  useEffect(() => {
    if (!focusInstanceId) return;
    const el = document.getElementById(`wf-history-${focusInstanceId}`);
    el?.scrollIntoView({ behavior: "smooth", block: "center" });
  }, [focusInstanceId, instances]);

  if (isLoading) return <LoadingState label="Loading history…" />;
  if (instances.length === 0) return <EmptyState label="You have no workflow history yet." />;

  return (
    <div className="mvx-admin-stack">
      {instances.map((inst: HistoryInstance) => (
        <div
          key={inst.id}
          id={`wf-history-${inst.id}`}
          className="mvx-admin-object"
          style={inst.id === focusInstanceId ? { boxShadow: "0 0 0 2px var(--color-brand-600)" } : undefined}
        >
          <div className="mvx-admin-object__header">
            <div className="mvx-admin-object__title">
              <span className="mvx-admin-object__name">{inst.workflow_name}</span>
              {inst.context?.revision && (
                <span className="mvx-admin-muted" style={{ marginLeft: 8 }}>
                  {inst.context.revision}
                </span>
              )}
            </div>
            <span className="mvx-admin-muted">{new Date(inst.started_at).toLocaleDateString()}</span>
            <StatusBadge tone={WF_STATUS_TONE[inst.status] ?? "neutral"}>{inst.status}</StatusBadge>
          </div>
          <div className="mvx-admin-object__body" style={{ gap: 6 }}>
            <TaskContextSummary contextDisplay={inst.context_display} context={inst.context} />
            {(inst.steps ?? []).map((s: HistoryStep, i: number) => (
              <div key={s.id} className="mvx-wf-step">
                <span className="mvx-wf-step__index">{i + 1}</span>
                <StepTypeIcon type={s.step_type} size={14} />
                <span className="mvx-wf-step__name">{s.step_name || s.step_def_id}</span>
                {s.decision && (
                  <StatusBadge tone={WF_DECISION_TONE[s.decision] ?? "neutral"}>{s.decision}</StatusBadge>
                )}
                {s.comment && s.comment !== "Auto-dispatched" && (
                  <span className="mvx-wf-step__comment">"{s.comment}"</span>
                )}
                <StatusBadge tone={WF_STEP_STATUS_TONE[s.status] ?? "neutral"}>{s.status}</StatusBadge>
              </div>
            ))}
          </div>
        </div>
      ))}
    </div>
  );
}
