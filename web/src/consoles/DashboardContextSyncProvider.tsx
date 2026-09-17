import { useCallback, useMemo, useRef, useState, type ReactNode } from "react";
import { DashboardContextSyncContext } from "./dashboardContextSync";

export function DashboardContextSyncProvider({ children }: { children: ReactNode }) {
  const [values, setValues] = useState<Record<string, string>>({});
  const setValue = useCallback((dimId: string, code: string) => {
    setValues(prev => (prev[dimId] === code ? prev : { ...prev, [dimId]: code }));
  }, []);

  // Selector-dedup claims: dimension id -> owning widget id. A ref, not
  // state — claims are decided during render (tree order) and never need to
  // trigger one themselves.
  const claims = useRef(new Map<string, string>());
  const claimSelector = useCallback((dimId: string, widgetId: string) => {
    const owner = claims.current.get(dimId);
    if (owner !== undefined && owner !== widgetId) return false;
    claims.current.set(dimId, widgetId);
    return true;
  }, []);
  const releaseSelectors = useCallback((widgetId: string) => {
    for (const [dimId, owner] of claims.current) {
      if (owner === widgetId) claims.current.delete(dimId);
    }
  }, []);

  const value = useMemo(
    () => ({ values, setValue, claimSelector, releaseSelectors }),
    [values, setValue, claimSelector, releaseSelectors],
  );
  return <DashboardContextSyncContext.Provider value={value}>{children}</DashboardContextSyncContext.Provider>;
}
