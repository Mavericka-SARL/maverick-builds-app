import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { LayoutDashboard, Inbox, History, Shield, KeyRound, Boxes } from "lucide-react";
import { api, type DemoContext } from "../../api/client";
import { DashboardsView } from "../business/DashboardsView";
import { AppsTab } from "../business/AppsTab";
import { PageLayout, LoadingState } from "../../ui";
import { tabId, localTab, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";
import { WorkflowInbox, WorkflowHistory, RolesTab, AccessRulesTab } from "./BusinessAdminConsole";

type Tab = "dashboards" | "inbox" | "history" | "roles" | "access" | "models";

const TAB_LABELS: Record<Tab, string> = {
  dashboards: "Dashboards",
  inbox: "Workflow Inbox",
  history: "History",
  roles: "Roles",
  access: "Access Rules",
  models: "Models",
};

const SECTION: SectionId = "business-admin";

/**
 * The business_admin section of the console: Run › Dashboards, Workflow
 * Inbox and Business Admin › History, Roles, Access Rules, Models. Forms have
 * no screen of their own: their records are worked on through the form
 * widgets a developer places on dashboards, as business users do. A tenant
 * admin's own group (Applications with model export/import, Users, Audit
 * Log) comes from the platform-admin module and appears right after these.
 */
export function useBusinessAdminSection({ enabled, setTab }: SectionInput): ConsoleSection | null {
  const [focusInstanceId, setFocusInstanceId] = useState<string | undefined>();
  const { data: ctx, isLoading: ctxLoading } = useQuery({ queryKey: ["demo"], queryFn: api.getDemo, enabled });

  if (!enabled) return null;
  const t = (id: Tab) => tabId(SECTION, id);
  return {
    id: SECTION,
    navGroups: [
      {
        label: "Run",
        items: [
          { id: t("dashboards"), label: "Dashboards", icon: <LayoutDashboard size={16} /> },
          { id: t("inbox"), label: "Workflow Inbox", icon: <Inbox size={16} /> },
        ],
      },
      {
        label: "Business Admin",
        items: [
          { id: t("history"), label: "History", icon: <History size={16} /> },
          { id: t("roles"), label: "Roles", icon: <Shield size={16} /> },
          { id: t("access"), label: "Access Rules", icon: <KeyRound size={16} /> },
          { id: t("models"), label: "Models", icon: <Boxes size={16} /> },
        ],
      },
    ],
    contextItems: ctx ? [{ id: "revision", label: "Revision", value: ctx.revision, tone: "live" as const }] : [],
    onNotificationNavigate: (resourceType, resourceId) => {
      if (resourceType !== "workflow_instance") return false;
      setTab(t("history"));
      setFocusInstanceId(resourceId);
      return true;
    },
    render: (active) => {
      const cur = localTab(active) as Tab;
      return (
        <PageLayout title={cur === "dashboards" ? undefined : TAB_LABELS[cur]}>
          {ctxLoading && cur === "dashboards" && <LoadingState label="Connecting…" />}
          {cur === "dashboards" && ctx && <DashboardsView ctx={ctx as DemoContext} />}
          {cur === "inbox" && <WorkflowInbox />}
          {cur === "history" && <WorkflowHistory focusInstanceId={focusInstanceId} />}
          {cur === "roles" && <RolesTab />}
          {cur === "access" && <AccessRulesTab />}
          {cur === "models" && <AppsTab />}
        </PageLayout>
      );
    },
  };
}
