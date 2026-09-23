/**
 * Where a link to a published document should open. A document the operator
 * hosts on its own site leaves this app, so it gets its own tab; the shipped
 * pages at /terms and /privacy stay in the current one.
 *
 * Component-free on purpose: react-refresh rejects a .tsx that exports both a
 * component and a plain helper.
 */
export function externalAttrs(href: string): { target?: string; rel?: string } {
  return /^https?:\/\//i.test(href) ? { target: "_blank", rel: "noreferrer" } : {};
}
