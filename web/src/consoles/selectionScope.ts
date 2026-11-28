// What a dashboard's selection leaves of a dimension: the members a grid whose
// rows or columns follow the selection shows, and the members a records
// widget's followed field may hold (widget_props follow_rows / follow_columns,
// record_follows).
//
// A member is under a selected member when it is the member itself or one of
// its descendants in the same dimension; when its dimension is a declared
// child of the selected member's dimension (parent_dimension_id: Cabinet under
// Department) and its chain of parents reaches the selected member, over any
// number of levels (Division → Department → Cabinet); or when its dimension is
// grouped by the selected member's dimension (source_dimension_id /
// source_property) and its property holds that member's code — or the code of
// a member the selection leaves of that grouping, so a grouping of a grouping
// narrows too: a project selected leaves its activities, and the cost lines
// whose activity is one of them (2026-11-27: a project dashboard's cost-line
// grid showed all 576 lines of the portfolio). Selections are
// read from the top ancestor down; one that is no longer under the selection
// above it (a department of another division) is left out, so a new choice
// higher up never empties what follows.

export interface ScopeMember {
  code: string;
  /** The parent member's code: in the same dimension, or, for a top member of a declared child dimension, in its parent dimension. */
  parent_code?: string | null;
  properties?: Record<string, string>;
}

export interface ScopeDim {
  id: string;
  parent_dimension_id?: string | null;
  source_dimension_id?: string | null;
  source_property?: string | null;
  members: ScopeMember[];
}

/**
 * The codes of dimId's members the selection leaves (null: every member — nothing applies). selected holds the
 * dashboard's member per dimension; a dimension in ignore (the widget's own clicks) does not narrow itself.
 */
export function selectionScope(dimId: string, dims: ScopeDim[], selected: Record<string, string>, ignore?: Set<string>): Set<string> | null {
  return scopeOf(dimId, dims, selected, ignore, new Set());
}

function scopeOf(dimId: string, dims: ScopeDim[], selected: Record<string, string>, ignore: Set<string> | undefined, seen: Set<string>): Set<string> | null {
  if (seen.has(dimId)) return null;
  seen = new Set(seen).add(dimId);
  const byId = new Map(dims.map(d => [d.id, d]));
  const dim = byId.get(dimId);
  if (!dim) return null;
  // The dimension and its declared ancestors, top first.
  const chain: ScopeDim[] = [dim];
  for (let p = dim.parent_dimension_id, guard = 0; p && guard < 20; guard++) {
    const pd = byId.get(p);
    if (!pd || chain.includes(pd)) break;
    chain.unshift(pd);
    p = pd.parent_dimension_id;
  }
  let allowed: Set<string> | null = null; // the previous level's members still in play
  for (let i = 0; i < chain.length; i++) {
    const d = chain[i];
    const codes = new Set(d.members.map(m => m.code));
    let here: Set<string> | null = null;
    if (i > 0 && allowed) {
      // The members whose chain leaves this dimension at an allowed member of the one above.
      const parentOf = new Map(d.members.map(m => [m.code, m.parent_code ?? ""]));
      const exit = (code: string) => {
        let c = code;
        for (let guard = 0; guard < 50; guard++) {
          const p = parentOf.get(c) ?? "";
          if (!p) return "";
          if (!codes.has(p)) return p;
          c = p;
        }
        return "";
      };
      here = new Set(d.members.filter(m => allowed!.has(exit(m.code))).map(m => m.code));
    }
    const sel = ignore?.has(d.id) ? undefined : selected[d.id];
    if (sel && codes.has(sel) && (!here || here.has(sel))) here = subtree(d, sel);
    allowed = here;
  }
  // A grouping of the dimension, selected: the members holding that group in its property.
  for (const g of dims) {
    if (g.source_dimension_id !== dimId || !g.source_property || ignore?.has(g.id)) continue;
    const sel = selected[g.id];
    let groups: Set<string> | null = null;
    if (sel && g.members.some(m => m.code === sel && !g.members.some(x => x.parent_code === m.code))) groups = new Set([sel]);
    else if (!sel) groups = scopeOf(g.id, dims, selected, ignore, seen); // what a selection further up leaves of the grouping
    if (!groups) continue;
    const grouped = new Set(dim.members.filter(m => groups!.has(m.properties?.[g.source_property!] ?? "")).map(m => m.code));
    allowed = allowed ? new Set([...allowed].filter(c => grouped.has(c))) : grouped;
  }
  // Everything left (the top member selected): nothing narrows.
  return allowed && allowed.size >= dim.members.length ? null : allowed;
}

/** code and every member below it in d. */
function subtree(d: ScopeDim, code: string): Set<string> {
  const children = new Map<string, string[]>();
  for (const m of d.members) if (m.parent_code) children.set(m.parent_code, [...(children.get(m.parent_code) ?? []), m.code]);
  const out = new Set<string>([code]);
  const walk = (c: string) => { for (const k of children.get(c) ?? []) if (!out.has(k)) { out.add(k); walk(k); } };
  walk(code);
  return out;
}
