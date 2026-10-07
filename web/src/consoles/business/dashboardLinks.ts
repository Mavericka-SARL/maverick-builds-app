import type { AppInfo, AppModelInfo } from "../../api/client";

/**
 * Links from a text widget to another dashboard, written in its Markdown as
 *
 *   [label](dashboard:Dashboard name)             a dashboard of the model open now
 *   [label](dashboard:Model name/Dashboard name)  a dashboard of another model
 *   [label](dashboard:Model name/)                another model, at its first dashboard
 *
 * By NAME, never by id, so a link outlives what ids do not: a revision copy
 * gives every dashboard a new id, and a model exported into another tenant,
 * or a starter imported on its own, gives every model one. Nothing is stored
 * that revision duplication or export would have to remap. Following a link
 * reads the same lists the Model switcher and the dashboard tabs read, so it
 * reaches only what the viewer could reach by hand; a renamed or hidden
 * target says so instead of opening something else.
 *
 * A name holding "/", ")" or "%" is written percent-encoded ("%2F", "%29",
 * "%25"); the designer's link picker does it.
 */
export const DASHBOARD_SCHEME = "dashboard:";

export interface DashboardLinkTarget {
  /** null: the model open now. */
  model: string | null;
  /** null: the model's first dashboard. */
  dashboard: string | null;
}

export function parseDashboardLink(href: string): DashboardLinkTarget | null {
  if (!href.startsWith(DASHBOARD_SCHEME)) return null;
  const body = href.slice(DASHBOARD_SCHEME.length);
  const slash = body.indexOf("/");
  const model = slash < 0 ? "" : decodePart(body.slice(0, slash));
  const dashboard = decodePart(slash < 0 ? body : body.slice(slash + 1));
  if (!model && !dashboard) return null;
  return { model: model || null, dashboard: dashboard || null };
}

export function dashboardLinkHref(model: string | null, dashboard: string | null): string {
  return DASHBOARD_SCHEME + (model ? `${encodePart(model)}/` : "") + (dashboard ? encodePart(dashboard) : "");
}

function decodePart(s: string): string {
  try {
    return decodeURIComponent(s).trim();
  } catch {
    return s.trim();
  }
}

function encodePart(s: string): string {
  return s.trim().replace(/[%/)]/g, c => `%${c.charCodeAt(0).toString(16).toUpperCase()}`);
}

/** Names match ignoring case and surrounding space, as a person reads them. */
export function sameName(a: string, b: string): boolean {
  return a.trim().toLowerCase() === b.trim().toLowerCase();
}

/** The model a link names: in the application open now if it has one, else the first anywhere. */
export function findModel(apps: AppInfo[], name: string, currentAppId: string): { app: AppInfo; model: AppModelInfo } | null {
  const hits = apps.flatMap(app => (app.models ?? []).filter(m => sameName(m.name, name)).map(model => ({ app, model })));
  return hits.find(h => h.app.id === currentAppId) ?? hits[0] ?? null;
}

// A link into another model reloads the console into it (modelSelection);
// the dashboard to open then waits here, for that model only.
const PENDING_KEY = "mvx.pendingDashboard";

export function setPendingDashboard(modelId: string, dashboard: string | null) {
  try {
    sessionStorage.setItem(PENDING_KEY, JSON.stringify({ modelId, dashboard }));
  } catch {
    // Storage blocked: the model still opens, at its first dashboard.
  }
}

/** The dashboard a link asked for in this model ("" = its first), or null. Read-only; clearPendingDashboard forgets it. */
export function peekPendingDashboard(modelId: string): string | null {
  try {
    const raw = sessionStorage.getItem(PENDING_KEY);
    if (!raw) return null;
    const p = JSON.parse(raw) as { modelId?: string; dashboard?: string | null };
    return p.modelId === modelId ? (p.dashboard ?? "") : null;
  } catch {
    return null;
  }
}

export function clearPendingDashboard() {
  try {
    sessionStorage.removeItem(PENDING_KEY);
  } catch {
    // nothing to forget
  }
}
