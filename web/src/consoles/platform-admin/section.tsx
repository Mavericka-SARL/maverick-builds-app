import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Boxes, Users, ScrollText, Server, KeyRound, Bell, Sparkles, Fingerprint, UserPlus, BarChart3, Palette, Layers } from "lucide-react";
import { api } from "../../api/client";
import { UsersPanel } from "../admin/UsersPanel";
import { InfraNodesTab } from "./InfraNodesTab";
import { LicenseTab } from "./LicenseTab";
import { PlansTab } from "./PlansTab";
import { SettingsScopePicker, DEPLOYMENT_SCOPE } from "./SettingsScopePicker";
import { NotificationSettingsTab } from "../admin/NotificationSettingsTab";
import { TenantAIKeysTab } from "../../ee/aikeys/TenantAIKeysTab";
import { SsoTab } from "../../ee/sso/SsoTab";
import { ScimTab } from "../../ee/scim/ScimTab";
import { UsageTab } from "../../ee/usage/UsageTab";
import { BrandingTab } from "../../ee/branding/BrandingTab";
import { computeAssignableRoles, canManageResourceAccess } from "../admin/roles";
import { PageLayout, LoadingState, ErrorState } from "../../ui";
import { tabId, localTab, sectionOf, type ConsoleSection, type SectionId, type SectionInput } from "../../router/sections";
import { ApplicationsView, AuditView } from "./PlatformAdminConsole";

/** Tabs whose content is one tenant's (or the deployment's) settings. */
const SCOPED_TABS = new Set<string>(["notifications", "ai-keys", "sso", "scim", "branding", "audit"]);
/** Of those, the ones with a deployment-wide row to inherit from. */
const DEPLOYMENT_ROW_TABS = new Set<string>(["notifications", "ai-keys", "audit"]);

/** Identity and branding are always one tenant's: at platform scope they wait for a choice. */
function tenantChosen(scope: "platform" | "tenant", settingsScope: string): boolean {
  return scope === "tenant" || settingsScope !== DEPLOYMENT_SCOPE;
}

type Tab = "applications" | "users" | "audit" | "usage" | "notifications" | "ai-keys" | "sso" | "scim" | "branding" | "plans" | "infra" | "license";
const TAB_LABELS: Record<Tab, string> = { applications: "Applications", users: "Users", audit: "Audit Log", usage: "Usage", notifications: "Notification delivery", "ai-keys": "AI keys", sso: "Single sign-on", scim: "Provisioning (SCIM)", branding: "Branding", plans: "Plans", infra: "Infrastructure", license: "License" };

/**
 * The administration section of the console — Applications (models,
 * revisions, model export/import), Users, Audit Log — at one of two scopes:
 * "platform" (platform_admin: every tenant, plus Infrastructure) or "tenant"
 * (tenant_admin: their own tenant, as the server scopes it). The tenant
 * flavour sits in the same console as the business-admin groups, because a
 * tenant admin is almost always also the business admin of their tenant and
 * a separate console put model export/import where they never looked
 * (reported live, 2026-09-10).
 */
export function useAdminSection({ enabled, tab, scope }: SectionInput & { scope: "platform" | "tenant" }): ConsoleSection | null {
  const section: SectionId = scope === "platform" ? "platform-admin" : "tenant-admin";
  const local = (sectionOf(tab) === section ? localTab(tab) : "") as Tab | "";
  const { data: me } = useQuery({ queryKey: ["admin-me"], queryFn: api.getAdminMe, enabled });
  const { data: tenants, isLoading: tenantsLoading, error: tenantsError } = useQuery({ queryKey: ["admin-tenants"], queryFn: api.getAdminTenants, enabled });
  const { data: users, isLoading: usersLoading, error: usersError } = useQuery({ queryKey: ["admin-users"], queryFn: api.getAdminUsers, enabled: enabled && local === "users" });
  const { data: audit, isLoading: auditLoading, error: auditError } = useQuery({ queryKey: ["admin-audit"], queryFn: api.getAdminAudit, enabled: enabled && local === "audit" });
  // Whose settings the per-tenant tabs show at platform scope: a tenant, or
  // the deployment's own row (SettingsScopePicker).
  const [settingsScope, setSettingsScope] = useState(DEPLOYMENT_SCOPE);

  if (!enabled) return null;
  const t = (id: Tab) => tabId(section, id);
  const roles = me?.roles ?? [];
  const isPlatformAdmin = roles.includes("platform_admin");
  const isTenantAdmin = roles.includes("tenant_admin");

  const items = [
    { id: t("applications"), label: TAB_LABELS.applications, icon: <Boxes size={16} /> },
    { id: t("users"), label: TAB_LABELS.users, icon: <Users size={16} /> },
    { id: t("audit"), label: TAB_LABELS.audit, icon: <ScrollText size={16} /> },
    // Usage is per tenant: every tenant at the platform scope, one's own otherwise.
    { id: t("usage"), label: TAB_LABELS.usage, icon: <BarChart3 size={16} /> },
    // Outbound delivery is per tenant, so both scopes get it.
    { id: t("notifications"), label: TAB_LABELS.notifications, icon: <Bell size={16} /> },
    // So is the AI key. The tab always shows: on a non-enterprise edition it
    // renders the feature gate, which is how an admin learns the option
    // exists at all.
    { id: t("ai-keys"), label: TAB_LABELS["ai-keys"], icon: <Sparkles size={16} /> },
    // Identity is per tenant too: its own sign-in provider and directory.
    { id: t("sso"), label: TAB_LABELS.sso, icon: <Fingerprint size={16} /> },
    { id: t("scim"), label: TAB_LABELS.scim, icon: <UserPlus size={16} /> },
    { id: t("branding"), label: TAB_LABELS.branding, icon: <Palette size={16} /> },
  ];
  if (scope === "platform") {
    // Plans are the platform's catalog: what each tenant may use.
    items.push({ id: t("plans"), label: TAB_LABELS.plans, icon: <Layers size={16} /> });
    items.push({ id: t("infra"), label: TAB_LABELS.infra, icon: <Server size={16} /> });
    // The license is deployment-wide, so only the platform scope shows it.
    items.push({ id: t("license"), label: TAB_LABELS.license, icon: <KeyRound size={16} /> });
  }

  return {
    id: section,
    navGroups: [{ label: scope === "platform" ? "Platform" : "Tenant admin", items }],
    // No identity item: the sidebar heading already names the console and the
    // account button carries the name and address.
    contextItems: [],
    render: (active) => {
      const cur = localTab(active) as Tab;
      const isLoading = cur === "applications" ? tenantsLoading : cur === "users" ? usersLoading : cur === "audit" ? auditLoading : false;
      const error = cur === "applications" ? tenantsError : cur === "users" ? usersError : cur === "audit" ? auditError : null;
      const meta =
        cur === "users" && users ? `${users.length} users` :
        cur === "audit" && audit ? `${audit.length} events` :
        undefined;
      return (
        <PageLayout title={TAB_LABELS[cur]} meta={meta}>
          {isLoading && <LoadingState />}
          {error && <ErrorState message={(error as Error).message} />}
          {!isLoading && !error && cur === "applications" && tenants && <ApplicationsView tenants={tenants} isPlatformAdmin={isPlatformAdmin} canTransferModels={isTenantAdmin || isPlatformAdmin} />}
          {!isLoading && !error && cur === "users" && users && (
            <UsersPanel users={users} tenants={tenants ?? []} assignableRoles={computeAssignableRoles(roles)} canManageResourceAccess={canManageResourceAccess(roles)} currentUserId={me?.user_id} />
          )}
          {!isLoading && !error && cur === "audit" && audit && <AuditView events={audit} />}
          {scope === "platform" && SCOPED_TABS.has(cur) && (
            <SettingsScopePicker tenants={tenants ?? []} value={settingsScope} onChange={setSettingsScope} deploymentRow={DEPLOYMENT_ROW_TABS.has(cur)} />
          )}
          {cur === "notifications" && <NotificationSettingsTab />}
          {cur === "ai-keys" && <TenantAIKeysTab />}
          {cur === "usage" && <UsageTab scope={scope} />}
          {cur === "sso" && tenantChosen(scope, settingsScope) && <SsoTab />}
          {cur === "scim" && tenantChosen(scope, settingsScope) && <ScimTab />}
          {cur === "branding" && tenantChosen(scope, settingsScope) && <BrandingTab />}
          {cur === "plans" && scope === "platform" && <PlansTab />}
          {cur === "infra" && scope === "platform" && <InfraNodesTab />}
          {cur === "license" && scope === "platform" && <LicenseTab />}
        </PageLayout>
      );
    },
  };
}
