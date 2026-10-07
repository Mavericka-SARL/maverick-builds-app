import { useCallback, useState } from "react";

const STORAGE_KEY = "mvx-collapsed";

function readCollapsed(): Set<string> {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    return new Set(raw ? (JSON.parse(raw) as string[]) : []);
  } catch {
    return new Set();
  }
}

/**
 * Which rows of a tree (an application's models, a model's revisions) this
 * viewer has collapsed. Everything starts expanded; a choice is remembered in
 * this browser across visits. Keys are the caller's ("app:<id>", "model:<id>").
 */
export function useCollapsed(): { isCollapsed: (key: string) => boolean; toggle: (key: string) => void } {
  const [collapsed, setCollapsed] = useState<Set<string>>(readCollapsed);
  const toggle = useCallback((key: string) => {
    setCollapsed((prev) => {
      const next = new Set(readCollapsed());
      for (const k of prev) next.add(k);
      if (prev.has(key)) next.delete(key);
      else next.add(key);
      try {
        localStorage.setItem(STORAGE_KEY, JSON.stringify([...next]));
      } catch {
        // Storage unavailable: the choice holds for this visit only.
      }
      return next;
    });
  }, []);
  const isCollapsed = useCallback((key: string) => collapsed.has(key), [collapsed]);
  return { isCollapsed, toggle };
}
