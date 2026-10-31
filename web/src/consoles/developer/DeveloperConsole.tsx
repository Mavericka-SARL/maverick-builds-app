import { useState, useMemo, useEffect } from "react";
import { useQuery } from "@tanstack/react-query";
import { Boxes, Users, UserCog, Sigma, ListTree, GitBranch, Zap, FileText, Table2, LayoutDashboard, Workflow, Plug, Sparkles } from "lucide-react";
import { api, type AdminTenant, type AdminUser, type ApiError } from "../../api/client";
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
import { RolesTab } from "../business-admin/BusinessAdminConsole";
import { PageLayout, LoadingState, ErrorState, SELECTED_APP_KEY, useConfirm } from "../../ui";
import { setLiveEditGuard } from "../../api/liveEdit";
import { useModelSwitches } from "../business/modelSelection";
import { tabId, localTab, sectionOf, adminProvidesUsers, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";

// ── Section ───────────────────────────────────────────────────────────────────

type Tab = "applications" | "users" | "metrics" | "dimensions" | "graph" | "automation" | "forms" | "grids" | "dashboards" | "workflows" | "roles" | "integrations" | "ai";

const TAB_LABELS: Record<Tab, string> = {
  applications: "Developer Models",
  users:        "Users",
  metrics:      "Metrics",
  dimensions:   "Dimensions",
  graph:        "Dependency Graph",
  automation:   "Triggers",
  forms:        "Records",
  grids:        "Grids",
  dashboards:   "Dashboards",
  workflows:    "Workflows",
  roles:        "Roles",
  integrations: "Integrations",
  ai:           "AI Developer",
};

const SECTION: SectionId = "developer";

// The revision being built, kept per application across a reload. Held in
// memory only, a reload reopened Build on the application's default model,
// whatever model was being built (2026-10-23).
function workingRevisionKey(): string {
  return `working_revision:${localStorage.getItem(SELECTED_APP_KEY) ?? ""}`;
}

function storedWorkingRevision(): { id: string; name: string } {
  try {
    const v = JSON.parse(localStorage.getItem(workingRevisionKey()) ?? "null") as { id?: unknown; name?: unknown } | null;
    if (v && typeof v.id === "string" && typeof v.name === "string") return { id: v.id, name: v.name };
  } catch { /* unreadable or unavailable: no revision kept */ }
  return { id: "", name: "" };
}

/**
 * The developer section of the console: Developer › (model authoring) and, unless
 * a tenant/platform admin section already supplies it, Govern › Users.
 */
export function useDeveloperSection({ enabled, tab, setTab, roles }: SectionInput): ConsoleSection | null {
  const local = (sectionOf(tab) === SECTION ? localTab(tab) : "") as Tab | "";
  const [revisionId, setRevisionId] = useState<string>(() => storedWorkingRevision().id);
  const [revisionName, setRevisionName] = useState<string>(() => storedWorkingRevision().name);
  // The chosen revision when the model read refuses it (a kept one deleted
  // since, or no longer this person's to build): Build opens on the
  // application's default instead.
  const [goneRevisionId, setGoneRevisionId] = useState("");
  // A model switch in place (modelSelection) may open another application:
  // Build then opens that application's kept revision, as a reload did.
  const switches = useModelSwitches();
  const [seenSwitches, setSeenSwitches] = useState(switches);
  if (seenSwitches !== switches) {
    setSeenSwitches(switches);
    const kept = storedWorkingRevision();
    setRevisionId(kept.id);
    setRevisionName(kept.name);
    setGoneRevisionId("");
  }

  const handleSelectRevision = (id: string, name: string) => {
    setRevisionId(id);
    setRevisionName(name);
    setGoneRevisionId("");
    try {
      localStorage.setItem(workingRevisionKey(), JSON.stringify({ id, name }));
    } catch { /* kept for this page only */ }
  };

  const { data: autoRevisions, isLoading: autoRevisionsLoading } = useQuery({
    queryKey: ["dev-revisions-auto"],
    queryFn: () => api.getDevRevisions(),
    enabled,
  });

  const autoDefaultRevision = useMemo(() => {
    if (!autoRevisions || !autoRevisions.length) return null;
    return autoRevisions.find((r: { is_active: boolean }) => r.is_active) ?? autoRevisions[0];
  }, [autoRevisions]);

  const chosen = revisionId !== "" && revisionId !== goneRevisionId;
  const effectiveRevisionId = (chosen ? revisionId : "") || (autoDefaultRevision as { id: string } | null)?.id || "";
  const effectiveRevisionName = (chosen ? revisionName : "") || (autoDefaultRevision as { name: string } | null)?.name || "";

  const { data: model, isLoading: modelLoading, error: modelError } = useQuery({
    queryKey: ["dev-model", effectiveRevisionId],
    queryFn: () => api.getDevModel(effectiveRevisionId || undefined).catch((e: Partial<ApiError>) => {
      if (chosen && effectiveRevisionId === revisionId && (e.status ?? 0) >= 400 && (e.status ?? 0) < 500) {
        try {
          localStorage.removeItem(workingRevisionKey());
        } catch { /* nothing kept */ }
        setGoneRevisionId(revisionId);
      }
      throw e;
    }),
    enabled,
  });

  // Whether Build is working in the model's live revision: then the badge
  // says so, and the first change of the session asks first (decided
  // 2026-10-06) — business users see such an edit at once.
  const { data: modelRevisions } = useQuery({
    queryKey: ["dev-revisions", model?.model_id],
    queryFn: () => api.getDevRevisions(model!.model_id),
    enabled: enabled && !!model?.model_id,
  });
  const editingLive = !!effectiveRevisionId && !!modelRevisions?.some(r => r.id === effectiveRevisionId && r.is_active);
  const { confirm: confirmLive, confirmElement: liveConfirmElement } = useConfirm();
  useEffect(() => {
    if (!enabled || !editingLive) return;
    const key = `live_edit_ok:${effectiveRevisionId}`;
    setLiveEditGuard(() => {
      try {
        if (sessionStorage.getItem(key)) return Promise.resolve(true);
      } catch { /* ask */ }
      return new Promise<boolean>(resolve => confirmLive({
        title: "Edit the live revision?",
        body: `"${effectiveRevisionName}" is the revision business users work in now: what you change here, they see at once. `
          + "To prepare changes first, make a new revision under Developer › Developer Models and build in it.",
        confirmLabel: "Edit the live revision",
        destructive: false,
        onConfirm: () => {
          try { sessionStorage.setItem(key, "1"); } catch { /* asked again next time */ }
          resolve(true);
        },
        onCancel: () => resolve(false),
      }));
    });
    return () => setLiveEditGuard(null);
    // confirmLive is a fresh function each render; the guard needs only the revision.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [enabled, editingLive, effectiveRevisionId, effectiveRevisionName]);

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

  const noFetch = local === "applications" || local === "users" || local === "automation" || local === "forms" || local === "grids" || local === "dashboards" || local === "integrations" || local === "workflows" || local === "roles";
  const isLoading = local === "users" ? userTenantsLoading || usersLoading : local === "dimensions" ? dimsLoading : noFetch ? false : modelLoading;
  const error = local === "users" ? userTenantsError || usersError : local === "dimensions" ? dimsError : noFetch ? null : modelError;

  const navGroups = [
    {
      label: "Developer",
      items: [
        { id: t("ai"), label: TAB_LABELS.ai, icon: <Sparkles size={16} /> },
        { id: t("metrics"), label: TAB_LABELS.metrics, icon: <Sigma size={16} /> },
        { id: t("dimensions"), label: TAB_LABELS.dimensions, icon: <ListTree size={16} /> },
        { id: t("forms"), label: TAB_LABELS.forms, icon: <FileText size={16} /> },
        { id: t("grids"), label: TAB_LABELS.grids, icon: <Table2 size={16} /> },
        { id: t("dashboards"), label: TAB_LABELS.dashboards, icon: <LayoutDashboard size={16} /> },
        { id: t("workflows"), label: TAB_LABELS.workflows, icon: <Workflow size={16} /> },
        // The roles workflow steps are assigned to. A business admin's own
        // Roles tab (with members) already covers it for someone who holds both.
        ...(roles.includes("business_admin") ? [] : [{ id: t("roles"), label: TAB_LABELS.roles, icon: <UserCog size={16} /> }]),
        { id: t("automation"), label: TAB_LABELS.automation, icon: <Zap size={16} /> },
        { id: t("integrations"), label: TAB_LABELS.integrations, icon: <Plug size={16} /> },
        { id: t("graph"), label: TAB_LABELS.graph, icon: <GitBranch size={16} /> },
        // "Models" until 2026-10-26, beside the User group's Models.
        { id: t("applications"), label: TAB_LABELS.applications, aliases: ["Models"], icon: <Boxes size={16} /> },
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
        ? { id: "revision", label: "Revision", value: editingLive ? `${effectiveRevisionName} (live)` : effectiveRevisionName, tone: editingLive ? "live" as const : "draft" as const }
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
            {liveConfirmElement}
            {cur === "workflows" ? <WorkflowsTab revisionId={effectiveRevisionId || undefined} /> : <AIAssistant revisionId={effectiveRevisionId || undefined} revisionName={effectiveRevisionName || undefined} onSelectRevision={handleSelectRevision} />}
          </div>
        );
      }
      return (
        <PageLayout title={TAB_LABELS[cur]}>
          {liveConfirmElement}
          {isLoading && <LoadingState />}
          {error && <ErrorState message={(error as Error).message} />}
          {cur === "applications" && <DevApplicationsTab revisionId={effectiveRevisionId} revisionName={effectiveRevisionName} defaultRevisionPending={autoRevisionsLoading} canCreate={roles.includes("tenant_admin") || roles.includes("platform_admin")} onSelect={handleSelectRevision} />}
          {!isLoading && !error && cur === "users" && <UsersPanel users={users} tenants={userTenants} assignableRoles={computeAssignableRoles(me?.roles ?? [])} canManageResourceAccess={canManageResourceAccess(me?.roles ?? [])} currentUserId={me?.user_id} rolesLoading={!me} />}
          {!isLoading && !error && cur === "metrics" && model && <MetricsTab model={model} revisionId={effectiveRevisionId || undefined} />}
          {!isLoading && !error && cur === "dimensions" && dims && <DimensionsView dims={dims} revisionId={effectiveRevisionId || undefined} />}
          {!isLoading && !error && cur === "graph" && model && <DepGraph model={model} grids={graphGrids} dims={dims ?? []} />}
          {cur === "automation" && <AutomationTab revisionId={effectiveRevisionId || undefined} />}
          {cur === "forms" && <FormsTab revisionId={effectiveRevisionId || undefined} />}
          {cur === "grids" && <GridsTab revisionId={effectiveRevisionId || undefined} />}
          {cur === "dashboards" && <DashboardsTab revisionId={effectiveRevisionId || undefined} />}
          {cur === "roles" && <RolesTab variant="developer" revisionId={effectiveRevisionId || undefined} />}
          {cur === "integrations" && <IntegrationsTab revisionId={effectiveRevisionId || undefined} />}
        </PageLayout>
      );
    },
  };
}
