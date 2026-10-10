import { KINDS, type Kind } from "./kinds";

/**
 * What an object of the tenancy tree is — tenant, application, model,
 * revision — as a small label before its name, explained on hover. The tree
 * nests four kinds of thing, and a name alone ("Getting started", "Learn the
 * platform") does not say which it is (reported 2026-10-10).
 */
export function KindTag({ kind }: { kind: Kind }) {
  const k = KINDS[kind];
  return (
    <span className="mvx-badge mvx-badge--neutral mvx-kind-tag" title={k.hint}>
      {k.label}
    </span>
  );
}
