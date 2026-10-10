import { useCallback, useState } from "react";

function read(storageKey: string): Set<string> | null {
  try {
    const raw = localStorage.getItem(storageKey);
    return new Set(raw ? (JSON.parse(raw) as string[]) : []);
  } catch {
    return null;
  }
}

function write(storageKey: string, keys: Set<string>) {
  try {
    localStorage.setItem(storageKey, JSON.stringify([...keys]));
  } catch {
    // Storage unavailable: the choice holds for this visit only.
  }
}

/**
 * Which groups of a long list this viewer has opened — the inverse of
 * useCollapsed: everything starts collapsed (a dimension's hundreds of
 * members, an access-rules table), and what is opened is remembered in this
 * browser under storageKey.
 */
export function useExpanded(storageKey: string): {
  isExpanded: (key: string) => boolean;
  toggle: (key: string) => void;
  setAll: (keys: string[], expanded: boolean) => void;
} {
  const [expanded, setExpanded] = useState<Set<string>>(() => read(storageKey) ?? new Set());
  const update = useCallback((change: (next: Set<string>) => void) => {
    // From storage when it works: another list on the page may have changed
    // it since this one read it. Outside a state updater, which React may
    // run twice.
    const next = read(storageKey) ?? new Set(expanded);
    change(next);
    write(storageKey, next);
    setExpanded(next);
  }, [storageKey, expanded]);
  const toggle = useCallback((key: string) => update((next) => {
    if (next.has(key)) next.delete(key);
    else next.add(key);
  }), [update]);
  const setAll = useCallback((keys: string[], open: boolean) => update((next) => {
    for (const k of keys) {
      if (open) next.add(k);
      else next.delete(k);
    }
  }), [update]);
  const isExpanded = useCallback((key: string) => expanded.has(key), [expanded]);
  return { isExpanded, toggle, setAll };
}
