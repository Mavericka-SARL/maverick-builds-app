import type { DashboardFolder } from "../../api/client";

// Dashboard folders are a parent_id tree. Both consoles only ever need it
// flattened into depth-first order with a display path, so that lives here
// rather than being re-derived in each view.

export interface FolderNode extends DashboardFolder {
  depth: number;
  /** Ancestors + self joined with " / ", for selects and group headers. */
  path: string;
}

// Guards against a cycle (a folder reparented under its own descendant) by
// only ever emitting a folder once — the same defensive stance the backend's
// own hierarchy walks take with their depth caps.
export function flattenFolders(folders: DashboardFolder[]): FolderNode[] {
  const byParent = new Map<string | null, DashboardFolder[]>();
  for (const f of folders) {
    const key = f.parent_id ?? null;
    const bucket = byParent.get(key);
    if (bucket) bucket.push(f);
    else byParent.set(key, [f]);
  }
  for (const bucket of byParent.values()) {
    bucket.sort((a, b) => a.name.localeCompare(b.name));
  }

  const out: FolderNode[] = [];
  const seen = new Set<string>();
  const walk = (parentID: string | null, depth: number, prefix: string) => {
    for (const f of byParent.get(parentID) ?? []) {
      if (seen.has(f.id)) continue;
      seen.add(f.id);
      const path = prefix ? `${prefix} / ${f.name}` : f.name;
      out.push({ ...f, depth, path });
      walk(f.id, depth + 1, path);
    }
  };
  walk(null, 0, "");

  // Orphans (parent deleted mid-flight, or a cycle) still have to be
  // reachable — otherwise the dashboards inside them become unreachable too.
  for (const f of folders) {
    if (!seen.has(f.id)) {
      seen.add(f.id);
      out.push({ ...f, depth: 0, path: f.name });
    }
  }
  return out;
}

/** Group label for dashboards that aren't filed anywhere. */
export const UNFILED_LABEL = "Unfiled";
