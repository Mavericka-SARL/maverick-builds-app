import type { AdminTenant } from "../../api/client";

/**
 * What a search on Applications shows, as keys "tenant:<id>", "app:<id>",
 * "model:<id>" and "revision:<id>"; null when the query is empty and
 * everything shows. A tenant, application or model whose name matches shows
 * with everything under it; a revision matches by name or by the start of its
 * id (the list shows the first eight characters). The parents of a match show
 * too, and "open:app:<id>" / "open:model:<id>" mark the ones that must be
 * expanded for the match to be seen, whatever the viewer collapsed.
 */
export function tenancyMatches(tenants: AdminTenant[], query: string): Set<string> | null {
  const q = query.trim().toLowerCase();
  if (!q) return null;
  const has = (name: string) => name.toLowerCase().includes(q);
  const shown = new Set<string>();
  for (const t of tenants) {
    const tenantHit = has(t.name);
    let tenantShown = tenantHit;
    for (const a of t.applications ?? []) {
      const appHit = tenantHit || has(a.name);
      let appShown = appHit;
      for (const m of a.models) {
        const modelHit = appHit || has(m.name);
        let modelShown = modelHit;
        for (const r of m.revisions ?? []) {
          if (!modelHit && !has(r.name) && !(q.length >= 4 && r.id.startsWith(q))) continue;
          shown.add(`revision:${r.id}`);
          if (!modelHit) {
            modelShown = true;
            shown.add(`open:model:${m.id}`);
          }
        }
        if (!modelShown) continue;
        shown.add(`model:${m.id}`);
        if (!appHit) {
          appShown = true;
          shown.add(`open:app:${a.id}`);
        }
      }
      if (!appShown) continue;
      shown.add(`app:${a.id}`);
      tenantShown = true;
    }
    if (tenantShown) shown.add(`tenant:${t.id}`);
  }
  return shown;
}
