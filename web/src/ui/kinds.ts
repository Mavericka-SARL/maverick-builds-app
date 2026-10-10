/**
 * The four kinds of object the tenancy tree nests, each with the one-line
 * explanation KindTag shows on hover (KindTag.tsx).
 */
export type Kind = "tenant" | "application" | "model" | "revision";

export const KINDS: Record<Kind, { label: string; hint: string }> = {
  tenant: { label: "Tenant", hint: "Your organisation's account: its people, plan and storage. It holds applications." },
  application: { label: "Application", hint: "A group of related models, such as one business area. People are given access application by application." },
  model: { label: "Model", hint: "One planning model: its dimensions, metrics, grids, dashboards and workflows. It holds revisions." },
  revision: { label: "Revision", hint: "A complete copy of a model with its data. The active revision is the one everyone works in." },
};

/** The hover explanation of a kind, for a heading that names it already. */
export function kindHint(kind: Kind): string {
  return KINDS[kind].hint;
}
