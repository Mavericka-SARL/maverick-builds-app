import { useState, Suspense } from "react";
import { Workflow, Shield, KeyRound, Boxes, Copy, Snowflake } from "lucide-react";
import { useDemo } from "../../api/useDemo";
import { AppsTab } from "../business/AppsTab";
import { useModelLabel } from "../business/modelLabel";
import { PageLayout, LoadingState } from "../../ui";
import { tabId, localTab, pageTab, tabView, viewTab, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";
import { WorkflowViews, type WorkflowView } from "../WorkflowViews";
import { CopyValuesTab, FreezeValuesTab, WorkflowInbox, WorkflowHistory, RolesTab, AccessRulesTab } from "./screens";

type Tab = "workflow" | "roles" | "access" | "copy" | "freeze" | "models";

const TAB_LABELS: Record<Tab, string> = {
  workflow: "Workflow",
  roles: "Roles",
  access: "Access Rules",
  copy: "Copy values",
  freeze: "Freeze values",
  models: "Models",
};

const SECTION: SectionId = "business-admin";

/**
 * The business_admin section of the console: Business Admin › Workflow (the
 * Inbox of approvals it decides, and the History of every run it
 * administers), Roles, Access Rules, Copy values, Freeze values, Models — the Inbox and
 * Models only for someone without the user role, whose User group already
 * has them. Dashboards come from the user role alone. A tenant
 * admin's own group (Applications with model export/import, Users, Audit
 * Log) comes from the platform-admin module and appears right after these.
 */
export function useBusinessAdminSection({ enabled, setTab, roles }: SectionInput): ConsoleSection | null {
  const [focusInstanceId, setFocusInstanceId] = useState<string | undefined>();
  const { data: ctx } = useDemo(undefined, { enabled });
  const modelLabel = useModelLabel(ctx, enabled);

  if (!enabled) return null;
  const t = (id: Tab) => tabId(SECTION, id);
  // Dashboards come from the user role only. The inbox (the approvals a
  // business admin decides) and the model list are here for someone without
  // it; with it, the User group already has both.
  const isUser = roles.includes("business_user");
  const workflowView = (view: WorkflowView) => viewTab(t("workflow"), view === "inbox" || isUser ? "" : view);
  return {
    id: SECTION,
    navGroups: [
      {
        label: "Business Admin",
        items: [
          {
            id: t("workflow"), label: "Workflow", icon: <Workflow size={16} />,
            // The two pages it replaced: a dashboard's link to one opens its
            // view (the Inbox is User › Workflow's for someone with that role).
            views: [
              ...(isUser ? [] : [{ label: "Workflow Inbox", id: workflowView("inbox") }]),
              { label: "Workflow History", id: workflowView("history") },
            ],
          },
          { id: t("roles"), label: "Roles", icon: <Shield size={16} /> },
          { id: t("access"), label: "Access Rules", icon: <KeyRound size={16} /> },
          { id: t("copy"), label: "Copy values", icon: <Copy size={16} /> },
          { id: t("freeze"), label: "Freeze values", icon: <Snowflake size={16} /> },
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
      setTab(workflowView("history"));
      setFocusInstanceId(resourceId);
      return true;
    },
    render: (active) => {
      const cur = localTab(pageTab(active)) as Tab;
      const view: WorkflowView = isUser || tabView(active) === "history" ? "history" : "inbox";
      return (
        <PageLayout title={TAB_LABELS[cur]}>
          <Suspense fallback={<LoadingState />}>
          {cur === "workflow" && (
            <WorkflowViews
              view={view}
              onView={(v) => setTab(workflowView(v))}
              inbox={isUser ? undefined : <WorkflowInbox />}
              history={<WorkflowHistory focusInstanceId={focusInstanceId} />}
            />
          )}
          {cur === "roles" && <RolesTab builds={roles.includes("developer")} />}
          {cur === "access" && <AccessRulesTab />}
          {cur === "copy" && <CopyValuesTab />}
          {cur === "freeze" && <FreezeValuesTab />}
          {cur === "models" && <AppsTab />}
          </Suspense>
        </PageLayout>
      );
    },
  };
}
