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
  // metric_kpi used to be here: a KPI tile now stretches to its row and
  // takes its designed size_h as a minimum (DashboardWidgets.tsx), so a row
  // of tiles stays aligned as in Design and a tile carrying selectors grows
  // instead of clipping its value.
  "text",
]);

// ── Hierarchy-aware default member selection ────────────────────────────────
//
// Shared by PlanningGrid (business/BusinessConsole.tsx) and ChartWidget's
// context selectors so both pick the SAME default member for a dimension.
// Picking dim.members[0] directly would ignore hierarchy — it can be a
// parent (Q1), or a leaf of a later group. Walking the tree depth-first, in
// the dimension's own order, and taking the first LEAF respects the
// hierarchy instead.

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

// Siblings keep the order the API sends members in — the dimension's own
// order (time period, then sort_order, then code). Sorting
// them by code here showed selectors and default contexts alphabetically.
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
  function setLevels(nodes: MemberTreeNode<T>[], level: number) {
    nodes.forEach(n => { n.level = level; setLevels(n.children, level + 1); });
  }
  setLevels(roots, 0);
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
