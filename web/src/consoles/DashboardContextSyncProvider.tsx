import { useCallback, useMemo, useRef, useState, type ReactNode } from "react";
import { DashboardContextSyncContext } from "./dashboardContextSync";

// reserved: dimensions the dashboard's selector bar selects on — no widget
// claims their selector, every synced widget follows the bar. initial: the
// bar's opening members, in the shared pool from the first render so widgets
// adopt them instead of seeding their own defaults.
export function DashboardContextSyncProvider({ children, reserved, initial }: { children: ReactNode; reserved?: string[]; initial?: Record<string, string> }) {
  const [values, setValues] = useState<Record<string, string>>(() => ({ ...initial }));
  const setValue = useCallback((dimId: string, code: string) => {
    setValues(prev => (prev[dimId] === code ? prev : { ...prev, [dimId]: code }));
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
    () => ({ values, setValue, claimSelector, releaseSelectors, barDims }),
    [values, setValue, claimSelector, releaseSelectors, barDims],
  );
  return <DashboardContextSyncContext.Provider value={value}>{children}</DashboardContextSyncContext.Provider>;
}
