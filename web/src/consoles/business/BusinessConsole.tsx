import { useState, useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { invalidateModelData } from "../modelDataQueries";
import { useDemo } from "../../api/useDemo";
import { LayoutDashboard, Workflow, Boxes } from "lucide-react";
import { PageLayout, LoadingState, ErrorState } from "../../ui";
import { tabId, localTab, pageTab, tabView, viewTab, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";
import { WorkflowViews, type WorkflowView } from "../WorkflowViews";
import { WorkflowInbox } from "./WorkflowInboxTab";
import { WorkflowMyHistory } from "./WorkflowHistoryTab";
import { DashboardsView } from "./DashboardsView";
import { AppsTab } from "./AppsTab";
import { SELECTED_APP_KEY, useSelectModel } from "./modelSelection";
import { api } from "../../api/client";
import { useModelLabel } from "./modelLabel";

// ── Section ───────────────────────────────────────────────────────────────────

const SECTION: SectionId = "business";

/**
 * The user role's section of the console (key business_user): User ›
 * Dashboards, Workflow (its Inbox and History), Models — whatever other roles
 * the person holds (see router/sections.ts).
 */
export function useBusinessSection({ enabled, setTab }: SectionInput): ConsoleSection | null {
  const [focusInstanceId, setFocusInstanceId] = useState<string | undefined>();
  const qc = useQueryClient();
  const { data: ctx, isLoading, error } = useDemo(undefined, { enabled });
  const modelLabel = useModelLabel(ctx, enabled);
  const selectModel = useSelectModel();

  // Sync the user's primary app into localStorage so X-App-Id is always sent.
  // Without this, the first load has no selected_app_id and the backend falls back
  // to the globally "largest" model instead of the user's own app.
  useEffect(() => {
    if (!enabled || !ctx?.app_id) return;
    const stored = localStorage.getItem(SELECTED_APP_KEY);
    if (stored !== ctx.app_id) {
      localStorage.setItem(SELECTED_APP_KEY, ctx.app_id);
      qc.invalidateQueries({ queryKey: ["user-dashboards"] });
      invalidateModelData(qc);
    }
  }, [enabled, ctx?.app_id, qc]);

  if (!enabled) return null;
  const t = (id: string) => tabId(SECTION, id);
  const workflowView = (view: WorkflowView) => viewTab(t("workflow"), view === "inbox" ? "" : view);
  const openInstance = (instanceId: string) => {
    setTab(workflowView("history"));
    setFocusInstanceId(instanceId);
    // Workflow History lists the selected application's runs; a notification
    // may name another application's run: switch there to show it.
    void api.findMyWorkflowRun(instanceId).then((runs) => {
      const appId = runs[0]?.application_id;
      if (appId && appId !== localStorage.getItem(SELECTED_APP_KEY)) selectModel(appId, null);
    }).catch(() => { /* the run stays unfocused: nothing to switch to */ });
  };
  return {
    id: SECTION,
    navGroups: [{
      label: "User",
      items: [
        { id: t("dashboards"), label: "Dashboards", icon: <LayoutDashboard size={16} /> },
        {
          id: t("workflow"), label: "Workflow", icon: <Workflow size={16} />,
          // The two pages it replaced: a dashboard's link to one opens its view.
          views: [
            { label: "Workflow Inbox", id: workflowView("inbox") },
            { label: "Workflow History", id: workflowView("history") },
          ],
        },
        { id: t("models"), label: "Models", icon: <Boxes size={16} /> },
      ],
    }],
    contextItems: ctx ? [
      ...(modelLabel ? [{ id: "model", label: "Model", value: modelLabel }] : []),
      { id: "revision", label: "Revision", value: ctx.revision, tone: "live" as const },
    ] : [],
    onNotificationNavigate: (resourceType, resourceId) => {
      if (resourceType !== "workflow_instance") return false;
      openInstance(resourceId);
      return true;
    },
    render: (active) => {
      const local = localTab(pageTab(active));
      const view: WorkflowView = tabView(active) === "history" ? "history" : "inbox";
      return (
        // A dashboard scrolls sideways by itself; without a scroll box of
        // the page's own its selector bar stays in view as the window scrolls.
        <PageLayout className={local === "dashboards" ? "mvx-page--flow" : undefined}>
          {isLoading && <LoadingState label="Connecting…" />}
          {error && <ErrorState message={`Cannot reach gateway: ${(error as Error).message}`} />}
          {ctx && local === "dashboards" && (
            <DashboardsView
              ctx={ctx}
              // Same jump the notification centre uses, so a run started from a
              // dashboard button can be followed to its progress.
              onOpenInstance={openInstance}
            />
          )}
          {local === "workflow" && (
            <WorkflowViews
              view={view}
              onView={(v) => setTab(workflowView(v))}
              inbox={<>{ctx && <WorkflowInbox />}</>}
              history={<WorkflowMyHistory focusInstanceId={focusInstanceId} />}
            />
          )}
          {local === "models" && <AppsTab />}
        </PageLayout>
      );
    },
  };
}
