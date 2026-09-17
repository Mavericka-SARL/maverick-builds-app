import { createContext, useCallback, useContext, useEffect, useId, useMemo, useRef } from "react";
import type { Dispatch, SetStateAction } from "react";
import { defaultLeafCode, type LeafCodeMember } from "./dashboardLayout";

export interface DashboardContextSyncValue {
  values: Record<string, string>; // dimension id -> shared member code
  setValue: (dimId: string, code: string) => void;
  // Selector dedup: the first synced widget to claim a dimension renders
  // its selector; every other synced widget hides that dimension's selector
  // and just follows the shared value. Claiming is idempotent per widget.
  claimSelector: (dimId: string, widgetId: string) => boolean;
  releaseSelectors: (widgetId: string) => void;
}

export const DashboardContextSyncContext = createContext<DashboardContextSyncValue | undefined>(undefined);

// useWidgetContextSync mirrors a shared, dashboard-wide selection into a
// widget's OWN existing context/filterSel state, rather than returning a
// second "effective" value the caller has to remember to read from — every
// existing read of that state elsewhere in the widget (query keys,
// fullCombo, aria-live summaries) needs zero changes; only the setter
// passed to the selector's onChange changes. Called once per widget (not
// once per dimension — a per-dim hook inside a .map() would violate rules
// of hooks the moment the widget's own dim list changes size across
// renders, e.g. PlanningGrid's pivotContext on drag-reorder).
// useSelectorOwnership dedupes context selectors across a dashboard: the
// same dimension must not render a picker on every widget (reported live —
// two widgets each showing "period:" and "product:"). Returns a predicate:
// "should THIS widget render the selector for dimId?". Outside a sync
// provider, or with sync disabled, every widget owns all its selectors.
// Claims happen during render (idempotent per widget, so StrictMode's
// double render is safe) — tree order decides the owner; unmount releases.
export function useSelectorOwnership(syncEnabled: boolean): (dimId: string) => boolean {
  const sync = useContext(DashboardContextSyncContext);
  const widgetId = useId();
  // Dims this widget successfully claimed, so the effect can RE-claim them:
  // StrictMode runs setup→cleanup→setup on mount, and a cleanup that only
  // released would leave the registry empty until this widget's next render —
  // a window where any other widget re-rendering alone (poll refetch) steals
  // ownership and the selector hops between widgets. Re-claiming in setup
  // closes the window; a real unmount ends at the cleanup and stays released.
  const claimedRef = useRef<Set<string>>(new Set());
  useEffect(() => {
    if (!sync) return;
    for (const dimId of claimedRef.current) sync.claimSelector(dimId, widgetId);
    return () => sync.releaseSelectors(widgetId);
  }, [sync, widgetId]);
  const active = syncEnabled && sync !== undefined;
  return useCallback(
    (dimId: string) => {
      if (!active || !sync) return true;
      const owns = sync.claimSelector(dimId, widgetId);
      if (owns) claimedRef.current.add(dimId);
      return owns;
    },
    [active, sync, widgetId],
  );
}

// useSyncSetter lets a widget PUSH a member selection into the dashboard-wide
// context for a dimension it does not itself select on — a chart's plotted
// dimension, a grid's row/column axis. Clicking "EMEA" on a bar or "Germany"
// in a column header moves every synced selector for that dimension there
// (KPIs, other charts, grid filters). Returns null when the widget is not
// synced or no provider is mounted, which callers also use as "is
// click-to-select available here?".
export function useSyncSetter(syncEnabled: boolean): ((dimId: string, code: string) => void) | null {
  const sync = useContext(DashboardContextSyncContext);
  return useMemo(() => (syncEnabled && sync ? sync.setValue : null), [syncEnabled, sync]);
}

export function useWidgetContextSync<T extends { id: string; members: LeafCodeMember[] }>(
  dims: T[],
  syncEnabled: boolean,
  local: Record<string, string>,
  setLocal: Dispatch<SetStateAction<Record<string, string>>>,
): (dimId: string, code: string) => void {
  const sync = useContext(DashboardContextSyncContext);
  const active = syncEnabled && sync !== undefined;
  const dimIdsKey = dims.map(d => d.id).join(",");

  useEffect(() => {
    if (!active || !sync) return;
    for (const d of dims) {
      const shared = sync.values[d.id];
      if (shared === undefined) {
        // First synced widget to touch this dimension on this dashboard —
        // seed the shared pool from its own already-resolved default.
        // A local value that is not among the (viewer-visible) members —
        // a saved default pinned to a member hidden from this viewer — must
        // not become the dashboard-wide value.
        const own = local[d.id];
        const seed = (own && d.members.some(m => m.code === own) ? own : defaultLeafCode(d)) ?? "";
        if (seed) sync.setValue(d.id, seed);
      } else if (local[d.id] !== shared) {
        // Someone else already claimed this dimension — adopt it.
        setLocal(prev => (prev[d.id] === shared ? prev : { ...prev, [d.id]: shared }));
      }
    }
  // `local` is deliberately excluded — this should only re-evaluate when
  // the dim set or the shared pool changes, not on every local edit
  // (which is already broadcast via the returned setter below).
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dimIdsKey, active, sync?.values]);

  return useCallback(
    (dimId: string, code: string) => {
      setLocal(prev => (prev[dimId] === code ? prev : { ...prev, [dimId]: code }));
      if (active && sync) sync.setValue(dimId, code);
    },
    [active, sync, setLocal],
  );
}
