/**
 * One console, composed by role.
 *
 * Roles are additive, and the rule is that a combination of roles ADDS tabs
 * to a single console — it never opens a second console. Each role
 * contributes a section (one or more sidebar groups plus the screens behind
 * them); the console shows the union, in this order, and a user with every
 * role sees every group in one sidebar.
 *
 * Kept free of React so the mapping can be reasoned about (and tested) on
 * its own, separately from how the shell renders it.
 */
import type { ReactNode } from "react";
import type { ContextBarItem, NavGroup } from "../ui";

export type SectionId = "business" | "business-admin" | "developer" | "tenant-admin" | "platform-admin";

/** Sidebar order: everyday planning first, administration last. */
export const SECTION_ORDER: SectionId[] = ["business", "business-admin", "developer", "tenant-admin", "platform-admin"];

/**
 * Which sections a set of roles switches on. Where two roles would offer the
 * same screens the wider one wins, so nothing appears twice:
 * - business_admin's Plan/Admin groups already contain everything the
 *   business_user Plan group has (history moves under Admin).
 * - platform_admin's Applications/Users/Audit Log are the tenant_admin
 *   screens at platform scope.
 * - the developer's own Users tab is hidden once an admin section provides it.
 */
export function enabledSections(roles: string[]): SectionId[] {
  const has = (r: string) => roles.includes(r);
  const out: SectionId[] = [];
  if (has("business_user") && !has("business_admin")) out.push("business");
  if (has("business_admin")) out.push("business-admin");
  if (has("developer")) out.push("developer");
  if (has("tenant_admin") && !has("platform_admin")) out.push("tenant-admin");
  if (has("platform_admin")) out.push("platform-admin");
  return out;
}

/** True when an admin section (tenant or platform) supplies the Users screen. */
export function adminProvidesUsers(roles: string[]): boolean {
  return roles.includes("tenant_admin") || roles.includes("platform_admin");
}

/* The sidebar head used to name the widest section a person held
   ("Tenant administration"). It no longer does: the head carries the product
   mark for every role, and only white-labelling changes it (branding/
   BrandMark). Which sections someone holds is what the groups below it say. */

/** Tab ids are namespaced by section ("business-admin:history") so groups never collide. */
export function tabId(section: SectionId, tab: string): string {
  return `${section}:${tab}`;
}

export function sectionOf(tab: string): SectionId | null {
  const idx = tab.indexOf(":");
  return idx > 0 ? (tab.slice(0, idx) as SectionId) : null;
}

export function localTab(tab: string): string {
  const idx = tab.indexOf(":");
  return idx > 0 ? tab.slice(idx + 1) : tab;
}

/** The tab a section opens on when it is the landing one. */
export const SECTION_LANDING: Record<SectionId, string> = {
  business: tabId("business", "dashboards"),
  "business-admin": tabId("business-admin", "dashboards"),
  developer: tabId("developer", "applications"),
  "tenant-admin": tabId("tenant-admin", "applications"),
  "platform-admin": tabId("platform-admin", "applications"),
};

/** A remembered/selected tab, or the landing tab when it belongs to no enabled section. */
export function resolveTab(tab: string, sections: SectionId[]): string {
  const owner = sectionOf(tab);
  if (owner && sections.includes(owner)) return tab;
  return sections.length ? SECTION_LANDING[sections[0]] : "";
}

/** What each section hook receives from the shell. */
export interface SectionInput {
  /** False when the user's roles do not grant this section; the hook then returns null and fetches nothing. */
  enabled: boolean;
  /** The console's active tab (namespaced). */
  tab: string;
  setTab: (tab: string) => void;
  roles: string[];
}

/** What each section hook returns to the shell. */
export interface ConsoleSection {
  id: SectionId;
  navGroups: NavGroup[];
  /** Shown in the context bar while one of this section's tabs is active. */
  contextItems?: ContextBarItem[];
  /** Notification-centre jump; return true when this section handled it. */
  onNotificationNavigate?: (resourceType: string, resourceId: string) => boolean;
  /** Renders the active tab (a namespaced id owned by this section). */
  render: (tab: string) => ReactNode;
}
