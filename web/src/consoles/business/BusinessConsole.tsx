import { useState, useEffect } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import { LayoutDashboard, Inbox, History as HistoryIcon, Boxes } from "lucide-react";
import { PageLayout, LoadingState, ErrorState } from "../../ui";
import { tabId, localTab, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";
import { WorkflowInbox } from "./WorkflowInboxTab";
import { WorkflowMyHistory } from "./WorkflowHistoryTab";
import { DashboardsView } from "./DashboardsView";
import { AppsTab, SELECTED_APP_KEY } from "./AppsTab";

// ── Section ───────────────────────────────────────────────────────────────────

const SECTION: SectionId = "business";

/**
 * The business_user section of the console: Plan › Dashboards, Workflow
 * Inbox, My History, Models. Superseded by the business-admin section for
 * users who also hold business_admin (see router/sections.ts).
 */
export function useBusinessSection({ enabled, setTab }: SectionInput): ConsoleSection | null {
  const [focusInstanceId, setFocusInstanceId] = useState<string | undefined>();
  const qc = useQueryClient();
  const { data: ctx, isLoading, error } = useQuery({ queryKey: ["demo"], queryFn: api.getDemo, enabled });

  // Sync the user's primary app into localStorage so X-App-Id is always sent.
  // Without this, the first load has no selected_app_id and the backend falls back
  // to the globally "largest" model instead of the user's own app.
  useEffect(() => {
    if (!enabled || !ctx?.app_id) return;
    const stored = localStorage.getItem(SELECTED_APP_KEY);
    if (stored !== ctx.app_id) {
      localStorage.setItem(SELECTED_APP_KEY, ctx.app_id);
      qc.invalidateQueries({ queryKey: ["user-dashboards"] });
      qc.invalidateQueries({ queryKey: ["grid"] });
    }
  }, [enabled, ctx?.app_id, qc]);

  if (!enabled) return null;
  const t = (id: string) => tabId(SECTION, id);
  const openInstance = (instanceId: string) => { setTab(t("history")); setFocusInstanceId(instanceId); };
  return {
    id: SECTION,
    navGroups: [{
      label: "Plan",
      items: [
        { id: t("dashboards"), label: "Dashboards", icon: <LayoutDashboard size={16} /> },
        { id: t("inbox"), label: "Workflow Inbox", icon: <Inbox size={16} /> },
        { id: t("history"), label: "My History", icon: <HistoryIcon size={16} /> },
        { id: t("models"), label: "Models", icon: <Boxes size={16} /> },
      ],
    }],
    contextItems: ctx ? [{ id: "revision", label: "Revision", value: ctx.revision, tone: "live" as const }] : [],
    onNotificationNavigate: (resourceType, resourceId) => {
      if (resourceType !== "workflow_instance") return false;
      openInstance(resourceId);
      return true;
    },
    render: (active) => {
      const local = localTab(active);
      return (
        <PageLayout>
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
          {ctx && local === "inbox" && <WorkflowInbox />}
          {local === "history" && <WorkflowMyHistory focusInstanceId={focusInstanceId} />}
          {local === "models" && <AppsTab />}
        </PageLayout>
      );
    },
  };
}
