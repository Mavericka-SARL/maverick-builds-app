import type { ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api/client";
import { Tabs } from "../ui";

export type WorkflowView = "inbox" | "history";

/**
 * The one Workflow page (owner, 2026-11-18: "merge Workflow Inbox and
 * Workflow History to one tab Workflow"): the Inbox — the steps waiting for
 * the person, counted on its tab — and the History of runs. A business admin
 * who also holds the user role decides in User › Workflow, so their Business
 * Admin › Workflow is the history alone (inbox omitted).
 */
export function WorkflowViews({ view, onView, inbox, history }: {
  view: WorkflowView;
  onView: (view: WorkflowView) => void;
  inbox?: ReactNode;
  history: ReactNode;
}) {
  // The Inbox's own query (same key): the count costs no extra request.
  const { data: tasks = [] } = useQuery({
    queryKey: ["tasks"],
    queryFn: api.getTasks,
    refetchInterval: 10_000,
    enabled: !!inbox,
  });
  if (!inbox) return <>{history}</>;
  return (
    <div className="mvx-admin-stack">
      <Tabs<WorkflowView>
        tabs={[{ id: "inbox", label: "Inbox", count: tasks.length }, { id: "history", label: "History" }]}
        active={view}
        onChange={onView}
      />
      {view === "inbox" ? inbox : history}
    </div>
  );
}
