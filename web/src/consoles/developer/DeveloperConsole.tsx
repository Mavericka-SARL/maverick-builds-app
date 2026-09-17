import { useState, useMemo } from "react";
import { useQuery } from "@tanstack/react-query";
import { Boxes, Users, Sigma, ListTree, GitBranch, Zap, FileText, Table2, LayoutDashboard, Workflow, Plug, Sparkles } from "lucide-react";
import { api, type AdminTenant, type AdminUser } from "../../api/client";
import { WorkflowsTab } from "./WorkflowsTab";
import { AIAssistant } from "./AIAssistant";
import { UsersPanel } from "../admin/UsersPanel";
import { computeAssignableRoles, canManageResourceAccess } from "../admin/roles";
import { DevApplicationsTab } from "./ApplicationsTab";
import { FormsTab } from "./FormsTab";
import { AutomationTab } from "./AutomationTab";
import { GridsTab } from "./GridsTab";
import { DimensionsView } from "./DimensionsTab";
import { MetricsTab } from "./MetricsTab";
import { DepGraph } from "./DependencyGraphTab";
import { IntegrationsTab } from "./IntegrationsTab";
import { DashboardsTab } from "./DashboardsTab";
import { PageLayout, LoadingState, ErrorState } from "../../ui";
import { tabId, localTab, sectionOf, adminProvidesUsers, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";

// ── Section ───────────────────────────────────────────────────────────────────

type Tab = "applications" | "users" | "metrics" | "dimensions" | "graph" | "automation" | "forms" | "grids" | "dashboards" | "workflows" | "integrations" | "ai";

const TAB_LABELS: Record<Tab, string> = {
  applications: "Models",
  users:        "Users",
  metrics:      "Metrics",
  dimensions:   "Dimensions",
  graph:        "Dependency Graph",
  automation:   "Triggers",
  forms:        "Forms",
  grids:        "Grids",
  dashboards:   "Dashboards",
  workflows:    "Workflows",
  integrations: "Integrations",
  ai:           "AI Developer",
};

const SECTION: SectionId = "developer";

/**
 * The developer section of the console: Build › (model authoring) and, unless
 * a tenant/platform admin section already supplies it, Govern › Users.
 */
export function useDeveloperSection({ enabled, tab, setTab, roles }: SectionInput): ConsoleSection | null {
  const local = (sectionOf(tab) === SECTION ? localTab(tab) : "") as Tab | "";
  const [revisionId, setRevisionId] = useState<string>("");
  const [revisionName, setRevisionName] = useState<string>("");

  const handleSelectRevision = (id: string, name: string) => {
    setRevisionId(id);
    setRevisionName(name);
  };

  const { data: autoRevisions } = useQuery({
    queryKey: ["dev-revisions-auto"],
    queryFn: () => api.getDevRevisions(),
    enabled,
  });

  const autoDefaultRevision = useMemo(() => {
    if (!autoRevisions || !autoRevisions.length) return null;
    return autoRevisions.find((r: { is_active: boolean }) => r.is_active) ?? autoRevisions[0];
  }, [autoRevisions]);

  const effectiveRevisionId = revisionId || (autoDefaultRevision as { id: string } | null)?.id || "";
  const effectiveRevisionName = revisionName || (autoDefaultRevision as { name: string } | null)?.name || "";

  const { data: model, isLoading: modelLoading, error: modelError } = useQuery({
    queryKey: ["dev-model", effectiveRevisionId],
    queryFn: () => api.getDevModel(effectiveRevisionId || undefined),
    enabled,
  });

  const { data: dims, isLoading: dimsLoading, error: dimsError } = useQuery({
    queryKey: ["dev-dimensions", effectiveRevisionId],
    queryFn: () => api.getDevDimensions(effectiveRevisionId || undefined),
    enabled,
  });

  const { data: graphGrids = [] } = useQuery({
    queryKey: ["dev-grids", effectiveRevisionId],
    queryFn: () => api.listGrids(effectiveRevisionId || undefined),
    enabled: enabled && local === "graph",
  });

  const { data: userTenants = [], isLoading: userTenantsLoading, error: userTenantsError } = useQuery<AdminTenant[]>({
    queryKey: ["developer-user-tenants"],
    queryFn: api.getDevApplications,
    enabled: enabled && local === "users",
  });

  const { data: users = [], isLoading: usersLoading, error: usersError } = useQuery<AdminUser[]>({
    queryKey: ["admin-users"],
    queryFn: api.getAdminUsers,
    enabled: enabled && local === "users",
  });

  // The shell shows who is signed in on every tab, and UserMenu shares this
  // exact query key.
  const { data: me } = useQuery({
    queryKey: ["me"],
    queryFn: api.getMe,
    enabled,
  });

  if (!enabled) return null;
  const t = (id: Tab) => tabId(SECTION, id);

  const noFetch = local === "applications" || local === "users" || local === "automation" || local === "forms" || local === "grids" || local === "dashboards" || local === "integrations" || local === "workflows";
  const isLoading = local === "users" ? userTenantsLoading || usersLoading : local === "dimensions" ? dimsLoading : noFetch ? false : modelLoading;
  const error = local === "users" ? userTenantsError || usersError : local === "dimensions" ? dimsError : noFetch ? null : modelError;

  const navGroups = [
    {
      label: "Build",
      items: [
        { id: t("applications"), label: TAB_LABELS.applications, icon: <Boxes size={16} /> },
        { id: t("metrics"), label: TAB_LABELS.metrics, icon: <Sigma size={16} /> },
        { id: t("dimensions"), label: TAB_LABELS.dimensions, icon: <ListTree size={16} /> },
        { id: t("forms"), label: TAB_LABELS.forms, icon: <FileText size={16} /> },
        { id: t("grids"), label: TAB_LABELS.grids, icon: <Table2 size={16} /> },
        { id: t("dashboards"), label: TAB_LABELS.dashboards, icon: <LayoutDashboard size={16} /> },
        { id: t("workflows"), label: TAB_LABELS.workflows, icon: <Workflow size={16} /> },
        { id: t("automation"), label: TAB_LABELS.automation, icon: <Zap size={16} /> },
        { id: t("integrations"), label: TAB_LABELS.integrations, icon: <Plug size={16} /> },
        { id: t("graph"), label: TAB_LABELS.graph, icon: <GitBranch size={16} /> },
        { id: t("ai"), label: TAB_LABELS.ai, icon: <Sparkles size={16} /> },
      ],
    },
  ];
  // An admin section (tenant or platform) already offers Users; showing a
  // second one here would be the same screen twice.
  if (!adminProvidesUsers(roles)) {
    navGroups.push({
      label: "Govern",
      items: [{ id: t("users"), label: TAB_LABELS.users, icon: <Users size={16} /> }],
    });
  }

  return {
    id: SECTION,
    navGroups,
    contextItems: [
      // Many models have a revision called "Working", and with nothing
      // selected the console falls back to a default revision that may belong
      // to another application. Naming the application and model beside the
      // badge is what tells the developer WHICH "Working" they are editing.
      ...(model?.app_name || model?.model_name
        ? [{ id: "model", label: "Model", value: [model.app_name, model.model_name].filter(Boolean).join(" · ") }]
        : []),
      effectiveRevisionId
        ? { id: "revision", label: "Revision", value: effectiveRevisionName, tone: "draft" as const }
        : { id: "revision", label: "Revision", value: "No revision selected", tone: "warning" as const },
    ],
    onNotificationNavigate: (resourceType) => {
      if (resourceType !== "workflow_instance") return false;
      setTab(t("workflows"));
      return true;
    },
    render: (active) => {
      const cur = localTab(active) as Tab;
      if (cur === "workflows" || cur === "ai") {
        return (
          <div style={{ flex: 1, overflow: "hidden" }}>
            {cur === "workflows" ? <WorkflowsTab revisionId={effectiveRevisionId || undefined} /> : <AIAssistant revisionId={effectiveRevisionId || undefined} revisionName={effectiveRevisionName || undefined} />}
          </div>
        );
      }
      return (
        <PageLayout title={TAB_LABELS[cur]}>
          {isLoading && <LoadingState />}
          {error && <ErrorState message={(error as Error).message} />}
          {cur === "applications" && <DevApplicationsTab revisionId={effectiveRevisionId} revisionName={effectiveRevisionName} onSelect={handleSelectRevision} />}
          {!isLoading && !error && cur === "users" && <UsersPanel users={users} tenants={userTenants} assignableRoles={computeAssignableRoles(me?.roles ?? [])} canManageResourceAccess={canManageResourceAccess(me?.roles ?? [])} currentUserId={me?.user_id} />}
          {!isLoading && !error && cur === "metrics" && model && <MetricsTab model={model} revisionId={effectiveRevisionId || undefined} />}
          {!isLoading && !error && cur === "dimensions" && dims && <DimensionsView dims={dims} revisionId={effectiveRevisionId || undefined} />}
          {!isLoading && !error && cur === "graph" && model && <DepGraph model={model} grids={graphGrids} dims={dims ?? []} />}
          {cur === "automation" && <AutomationTab />}
          {cur === "forms" && <FormsTab revisionId={effectiveRevisionId || undefined} />}
          {cur === "grids" && <GridsTab revisionId={effectiveRevisionId || undefined} />}
          {cur === "dashboards" && <DashboardsTab revisionId={effectiveRevisionId || undefined} />}
          {cur === "integrations" && <IntegrationsTab revisionId={effectiveRevisionId || undefined} />}
        </PageLayout>
      );
    },
  };
}
