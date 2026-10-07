import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Inbox, History, Shield, KeyRound, Boxes } from "lucide-react";
import { api } from "../../api/client";
import { AppsTab } from "../business/AppsTab";
import { useModelLabel } from "../business/modelLabel";
import { PageLayout } from "../../ui";
import { tabId, localTab, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";
import { WorkflowInbox, WorkflowHistory, RolesTab, AccessRulesTab } from "./BusinessAdminConsole";

type Tab = "inbox" | "history" | "roles" | "access" | "models";

const TAB_LABELS: Record<Tab, string> = {
  inbox: "Workflow Inbox",
  history: "History",
  roles: "Roles",
  access: "Access Rules",
  models: "Models",
};

const SECTION: SectionId = "business-admin";

/**
 * The business_admin section of the console: Business Admin › Workflow Inbox
 * (the approvals it decides), History, Roles, Access Rules, Models — the
 * inbox and Models only for someone without the user role, whose User group
 * already has them. Dashboards come from the user role alone. A tenant
 * admin's own group (Applications with model export/import, Users, Audit
 * Log) comes from the platform-admin module and appears right after these.
 */
export function useBusinessAdminSection({ enabled, setTab, roles }: SectionInput): ConsoleSection | null {
  const [focusInstanceId, setFocusInstanceId] = useState<string | undefined>();
  const { data: ctx } = useQuery({ queryKey: ["demo"], queryFn: api.getDemo, enabled });
  const modelLabel = useModelLabel(ctx, enabled);

  if (!enabled) return null;
  const t = (id: Tab) => tabId(SECTION, id);
  // Dashboards come from the user role only. The inbox (the approvals a
  // business admin decides) and the model list are here for someone without
  // it; with it, the User group already has both.
  const isUser = roles.includes("business_user");
  return {
    id: SECTION,
    navGroups: [
      {
        label: "Business Admin",
        items: [
          ...(isUser ? [] : [{ id: t("inbox"), label: "Workflow Inbox", icon: <Inbox size={16} /> }]),
          { id: t("history"), label: "History", icon: <History size={16} /> },
          { id: t("roles"), label: "Roles", icon: <Shield size={16} /> },
          { id: t("access"), label: "Access Rules", icon: <KeyRound size={16} /> },
          ...(isUser ? [] : [{ id: t("models"), label: "Models", icon: <Boxes size={16} /> }]),
        ],
      },
    ],
    contextItems: ctx ? [
      ...(modelLabel ? [{ id: "model", label: "Model", value: modelLabel }] : []),
      { id: "revision", label: "Revision", value: ctx.revision, tone: "live" as const },
    ] : [],
    onNotificationNavigate: (resourceType, resourceId) => {
      if (resourceType !== "workflow_instance") return false;
      setTab(t("history"));
      setFocusInstanceId(resourceId);
      return true;
    },
    render: (active) => {
      const cur = localTab(active) as Tab;
      return (
        <PageLayout title={TAB_LABELS[cur]}>
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
