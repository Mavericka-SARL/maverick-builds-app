import { useCallback, useMemo, useRef, useState, type ReactNode } from "react";
import { DashboardContextSyncContext } from "./dashboardContextSync";

// reserved: dimensions the dashboard's selector bar selects on — no widget
// claims their selector, every synced widget follows the bar. initial: the
// bar's opening members, in the shared pool from the first render so widgets
// adopt them instead of seeding their own defaults.
// followBar: told the slot of each widget that follows the bar.
export function DashboardContextSyncProvider({ children, reserved, initial, followBar }: { children: ReactNode; reserved?: string[]; initial?: Record<string, string>; followBar?: (slot: string) => void }) {
  const [values, setValues] = useState<Record<string, string>>(() => ({ ...initial }));
  const [trail, setTrail] = useState<Record<string, { code: string; source: string }[]>>({});
  // An empty code clears the dimension's selection (a grid's Total row clicked
  // where the dimension has no single top member): nothing is selected.
  const setValue = useCallback((dimId: string, code: string, source?: string) => {
    setValues(prev => {
      if (code === "") {
        if (!(dimId in prev)) return prev;
        const next = { ...prev };
        delete next[dimId];
        return next;
      }
      return prev[dimId] === code ? prev : { ...prev, [dimId]: code };
    });
    const src = source ?? "";
    setTrail(prev => {
      const cur = prev[dimId] ?? [];
      if (cur[0]?.source === src && cur[0]?.code === code) return prev;
      return { ...prev, [dimId]: [{ code, source: src }, ...cur.filter(t => t.source !== src)] };
    });
  }, []);

  const reservedKey = (reserved ?? []).join(",");
  const reservedSet = useMemo(() => new Set(reservedKey ? reservedKey.split(",") : []), [reservedKey]);

  // Selector-dedup claims: dimension id -> owning widget id. A ref, not
  // state — claims are decided during render (tree order) and never need to
  // trigger one themselves.
  const claims = useRef(new Map<string, string>());
  const claimSelector = useCallback((dimId: string, widgetId: string) => {
    if (reservedSet.has(dimId)) return false;
    const owner = claims.current.get(dimId);
    if (owner !== undefined && owner !== widgetId) return false;
    claims.current.set(dimId, widgetId);
    return true;
  }, [reservedSet]);
  const releaseSelectors = useCallback((widgetId: string) => {
    for (const [dimId, owner] of claims.current) {
      if (owner === widgetId) claims.current.delete(dimId);
    }
  }, []);

  const barDims = useMemo(() => [...reservedSet], [reservedSet]);
  const value = useMemo(
    () => ({ values, trail, setValue, claimSelector, releaseSelectors, barDims, followBar }),
    [values, trail, setValue, claimSelector, releaseSelectors, barDims, followBar],
  );
  return <DashboardContextSyncContext.Provider value={value}>{children}</DashboardContextSyncContext.Provider>;
}
