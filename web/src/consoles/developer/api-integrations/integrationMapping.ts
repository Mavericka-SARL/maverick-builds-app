// Mapping helpers: infer available fields (union of paths + types) from the
// test preview's records, so Step 5 can offer real source paths instead of
// free-typing. Pure functions — reusable and unit-testable.

export interface InferredField {
  path: string; // constrained path relative to a record, e.g. $.attrs.amount
  types: string[]; // union of observed JSON types
  sample: string;
}

// flattenRecord walks one record up to `depth` levels, producing candidate
// paths. Arrays are represented by their first element ([0]) — enough to
// map typical API shapes without inventing wildcards the backend refuses.
export function flattenRecord(rec: unknown, prefix = "$", depth = 4): { path: string; value: unknown }[] {
  if (depth < 0) return [];
  const out: { path: string; value: unknown }[] = [];
  if (rec !== null && typeof rec === "object" && !Array.isArray(rec)) {
    for (const [k, v] of Object.entries(rec as Record<string, unknown>)) {
      const p = `${prefix}.${k}`;
      if (v !== null && typeof v === "object") {
        if (Array.isArray(v)) {
          if (v.length > 0) out.push(...flattenRecord(v[0], `${p}[0]`, depth - 1));
        } else {
          out.push(...flattenRecord(v, p, depth - 1));
        }
      } else {
        out.push({ path: p, value: v });
      }
    }
  }
  return out;
}

function jsonType(v: unknown): string {
  if (v === null) return "null";
  if (Array.isArray(v)) return "array";
  return typeof v;
}

// inferFields unions the flattened paths of up to 20 preview records.
export function inferFields(records: unknown[]): InferredField[] {
  const byPath = new Map<string, InferredField>();
  for (const rec of records.slice(0, 20)) {
    for (const { path, value } of flattenRecord(rec)) {
      const f = byPath.get(path);
      const t = jsonType(value);
      if (!f) {
        byPath.set(path, { path, types: [t], sample: String(value ?? "") });
      } else if (!f.types.includes(t)) {
        f.types.push(t);
      }
    }
  }
  return [...byPath.values()].sort((a, b) => a.path.localeCompare(b.path));
}

// parsePreviewRecords extracts the record list from a test run's preview
// body using the configured records path (client-side mirror of the server's
// constrained lookup, for display only).
export function parsePreviewRecords(previewBody: string, recordsPath: string): unknown[] {
  let doc: unknown;
  try {
    doc = JSON.parse(previewBody);
  } catch {
    return [];
  }
  let target = doc;
  const path = recordsPath.trim().replace(/^\$\.?/, "");
  if (path !== "") {
    for (const rawSeg of path.split(".")) {
      let seg = rawSeg;
      const idxs: number[] = [];
      while (seg.includes("[")) {
        const i = seg.indexOf("[");
        const j = seg.indexOf("]", i);
        if (j < 0) return [];
        idxs.push(Number(seg.slice(i + 1, j)));
        seg = seg.slice(0, i) + seg.slice(j + 1);
      }
      if (seg !== "") {
        if (target === null || typeof target !== "object" || Array.isArray(target)) return [];
        target = (target as Record<string, unknown>)[seg];
      }
      for (const n of idxs) {
        if (!Array.isArray(target) || n >= target.length) return [];
        target = target[n];
      }
      if (target === undefined) return [];
    }
  }
  if (Array.isArray(target)) return target;
  if (target !== null && typeof target === "object") return [target];
  return [];
}
