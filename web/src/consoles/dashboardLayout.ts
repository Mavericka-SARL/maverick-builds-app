import type { DashboardWidget } from "../api/client";

const SNAP = 20;

// Widget types whose natural content height should NOT be stretched to
// match a taller sibling in the same row-group (see groupWidgetsIntoRows
// below) — small, intrinsic-content widgets that would otherwise balloon
// to fill the row (e.g. a button placed beside a tall form/grid widget).
// Everything else (grid/chart/form/import) fills the row's full height —
// a chart's Recharts ResponsiveContainer specifically needs a real
// stretched height to measure against, not just a min-height, or it
// renders at 0×0.
export const INTRINSIC_HEIGHT_WIDGET_TYPES = new Set([
  "automation_button",
  "integration_button",
  // metric_kpi used to be here: a designed KPI height is now exact, like a
  // grid's or chart's, so a row of tiles keeps the heights Design shows
  // (a tile with selectors no longer towers over its neighbours).
  "text",
]);

// ── Hierarchy-aware default member selection ────────────────────────────────
//
// Shared by PlanningGrid (business/BusinessConsole.tsx) and ChartWidget's
// context selectors so both pick the SAME default member for a dimension.
// Picking dim.members[0] directly would sort alphabetically across the
// whole flat member list, ignoring hierarchy — e.g. for a period dimension
// with Q1-Q4 parents and JAN-DEC children, "APR" sorts before "FEB"
// alphabetically even though FEB (a Q1 leaf) is chronologically/
// hierarchically first. Walking the tree depth-first and taking the first
// LEAF respects that hierarchy instead.

// LeafCodeMember is the minimal shape defaultLeafCode's tree-walk needs —
// just code/parent_code — so it works equally for a full DimMember (the
// planning grid) and the leaner ChartContextMember a chart's context
// selectors use (rollup now happens server-side, so that shape carries no
// id or other hierarchy internals).
export interface LeafCodeMember {
  code: string;
  parent_code?: string;
}

export interface MemberTreeNode<T extends LeafCodeMember> {
  member: T;
  level: number;
  children: MemberTreeNode<T>[];
}

export function buildMemberTree<T extends LeafCodeMember>(members: T[]): MemberTreeNode<T>[] {
  const byCode = new Map<string, MemberTreeNode<T>>();
  for (const m of members) {
    byCode.set(m.code, { member: m, level: 0, children: [] });
  }
  const roots: MemberTreeNode<T>[] = [];
  for (const node of byCode.values()) {
    const parent = node.member.parent_code ? byCode.get(node.member.parent_code) : undefined;
    if (parent) parent.children.push(node);
    else roots.push(node);
  }
  function sortRec(nodes: MemberTreeNode<T>[], level: number) {
    nodes.sort((a, b) => a.member.code.localeCompare(b.member.code));
    nodes.forEach(n => { n.level = level; sortRec(n.children, level + 1); });
  }
  sortRec(roots, 0);
  return roots;
}

function treeLeaves<T extends LeafCodeMember>(nodes: MemberTreeNode<T>[]): T[] {
  const out: T[] = [];
  function walk(n: MemberTreeNode<T>) {
    if (n.children.length === 0) out.push(n.member);
    else n.children.forEach(walk);
  }
  nodes.forEach(walk);
  return out;
}

// First leaf member's code, in the dimension's own hierarchical sort order
// — used for default context-zone selection, where picking a non-leaf
// (e.g. a same-dimension hierarchy's parent) would make every combo look
// like it touches an agg node.
export function defaultLeafCode<T extends LeafCodeMember>(dim: { members: T[] }): string | undefined {
  const leaves = treeLeaves(buildMemberTree(dim.members));
  return (leaves[0] ?? dim.members[0])?.code;
}

export function groupWidgetsIntoRows(widgets: DashboardWidget[]): DashboardWidget[][] {
  const sorted = [...widgets].sort((a, b) => (a.pos_y || 0) - (b.pos_y || 0) || (a.pos_x || 0) - (b.pos_x || 0));
  const rows: { minY: number; items: DashboardWidget[] }[] = [];
  for (const w of sorted) {
    const wy = w.pos_y || 0;
    const rowIdx = rows.findIndex(r => Math.abs(r.minY - wy) <= SNAP);
    if (rowIdx >= 0) {
      rows[rowIdx].items.push(w);
      rows[rowIdx].items.sort((a, b) => (a.pos_x || 0) - (b.pos_x || 0));
    } else {
      rows.push({ minY: wy, items: [w] });
    }
  }
  return rows.map(r => r.items);
}
