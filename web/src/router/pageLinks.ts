import { createContext } from "react";
import type { NavGroup } from "../ui/types";

/**
 * Links from a text widget to a page of the console's sidebar, written in
 * its Markdown as
 *
 *   [label](page:Workflow Inbox)              the page, in whichever group has it first
 *   [label](page:Developer › Dimensions)      the page of that group ("›" or ">")
 *
 * By the names the sidebar shows, never by an internal tab id, so a guide
 * reads like the console and a page added later is linkable at once. A link
 * resolves against the sidebar the reader actually has: a page their roles do
 * not give them is not opened — they are told so instead. Names match
 * ignoring case and surrounding space.
 */
export const PAGE_SCHEME = "page:";

export interface PageLinkTarget {
  /** The sidebar group's name; null: the first group that has the page. */
  group: string | null;
  page: string;
}

export function parsePageLink(href: string): PageLinkTarget | null {
  if (!href.startsWith(PAGE_SCHEME)) return null;
  let body = href.slice(PAGE_SCHEME.length);
  try {
    body = decodeURIComponent(body);
  } catch {
    /* written as is */
  }
  const parts = body.split(/[›>]/).map(s => s.trim());
  if (parts.some(p => !p) || parts.length > 2) return null;
  return parts.length === 2 ? { group: parts[0], page: parts[1] } : { group: null, page: parts[0] };
}

/** What the link names, as the reader would say it ("Developer › Dimensions"). */
export function pageLinkName(href: string): string {
  const t = parsePageLink(href);
  if (!t) return href.slice(PAGE_SCHEME.length).trim();
  return t.group ? `${t.group} › ${t.page}` : t.page;
}

const same = (a: string | undefined, b: string) => (a ?? "").trim().toLowerCase() === b.trim().toLowerCase();

/** The tab id of the sidebar page a link names, among the reader's own groups; null when they have none. */
export function resolvePageLink(target: PageLinkTarget, groups: NavGroup[]): string | null {
  for (const g of groups) {
    if (target.group !== null && !same(g.label, target.group)) continue;
    const item = g.items.find(i => !i.disabled && (same(i.label, target.page) || (i.aliases ?? []).some(a => same(a, target.page))));
    if (item) return item.id;
  }
  return null;
}

/**
 * Opens the sidebar page a "page:" link names; false when the reader's
 * console has no such page. Provided by the console shell (UnifiedConsole);
 * where nothing provides it (the dashboard designer and its Preview), such a
 * link still shows, and does nothing.
 */
export const ConsolePageContext = createContext<((href: string) => boolean) | null>(null);
