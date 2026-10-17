import { createContext } from "react";

/**
 * Follows an in-console link — "dashboard:…" (consoles/business/
 * dashboardLinks) or "page:…" (router/pageLinks) — from a RichText. The screen that can act on one provides
 * it (the Dashboards view); where nothing does (the dashboard designer and
 * its Preview), such a link still shows, and does nothing.
 */
export const InConsoleLinkContext = createContext<((href: string) => void) | null>(null);

/** Schemes that stay inside the console, followed through InConsoleLinkContext: a dashboard, or a sidebar page (router/pageLinks). */
export function isInConsoleHref(href: string): boolean {
  return href.startsWith("dashboard:") || href.startsWith("page:");
}
