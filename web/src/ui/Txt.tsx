import type { ReactNode } from "react";

/**
 * Text kept in an element of its own: text that comes and goes (a plural
 * "s", a " · via …" suffix), or that sits right after something that comes
 * and goes. A page translator (a browser's "translate this page") swaps each
 * bare text node for <font> elements; React, inserting before or removing a
 * text node it no longer finds, then failed the whole page ("Failed to
 * execute 'insertBefore' on 'Node'", a self-hosting tester, 2026-10-23).
 * Inside its own element the text is the element's content, which React
 * replaces whole. A plain inline span: one with display: contents laid out
 * the same, but gave it a space of its own in an accessible name ("result
 * s", "└ France"). In a flex parent it is an item of its own, the parent's
 * gap in place of a leading space. web/e2e/translation-safety.spec.ts finds
 * the places that need it.
 */
export function Txt({ children }: { children?: ReactNode }) {
  return <span className="mvx-txt">{children}</span>;
}
