import type { QueryClient } from "@tanstack/react-query";

// Every query whose result is derived from a revision's facts — grid cells
// and totals, KPI totals, chart series. Anything that writes facts (a cell
// writeback, a form record, an import, a metric change) must invalidate all
// of them together, or the widgets on the same dashboard disagree with each
// other until their own poll happens to fire: the grid showed 17 while the
// Revenue KPI next to it still said 15.
//
// Keep this list in step with the queryKeys the widgets use. `grid` is the
// legacy single-read key still used by the import and chart editors; the
// PlanningGrid reads `grid-meta` + `grid-cells` since the two-query split,
// which is why a bare ["grid"] invalidation no longer reaches it.
const MODEL_DATA_QUERY_KEYS = ["grid", "grid-meta", "grid-cells", "kpi-totals", "chart-data"] as const;

// Resolves once every matching ACTIVE query has refetched, so a caller can
// hold an optimistic value on screen until the server's answer has replaced
// it rather than until the request merely returned.
export function invalidateModelData(qc: QueryClient): Promise<void> {
  return Promise.all(MODEL_DATA_QUERY_KEYS.map((k) => qc.invalidateQueries({ queryKey: [k] }))).then(() => undefined);
}
