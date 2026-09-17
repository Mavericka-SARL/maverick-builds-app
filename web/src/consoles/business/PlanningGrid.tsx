import React, { useState, useEffect, useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { GripVertical, Download, MoreVertical, Rows3, X as XIcon } from "lucide-react";
import { api, type DemoContext, type GridData, type Metric, type DimMember, type DimInfo, type GridDefaultView , type SelectorsPosition } from "../../api/client";
import { CellHistoryDrawer, type CellRef } from "../../ee/cellhistory/CellHistoryDrawer";
import { LoadingState, ErrorState, Toolbar, ToolbarGroup, Select, PropertyPanel, IconButton } from "../../ui";
import { defaultLeafCode } from "../dashboardLayout";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { useSelectorOwnership, useSyncSetter, useWidgetContextSync } from "../dashboardContextSync";
import { downloadBlob } from "./blobUtils";

// ── Grid actions menu ─────────────────────────────────────────────────────────

// Export and Pivot data live under one ··· icon (reported live: two labeled
// buttons per grid widget ate dashboard space). Fixed-position dropdown from
// the trigger rect — the same pattern as WorkflowsTab's row menu — so the
// widget's own overflow container can't clip it.
function GridActionsMenu({
  canExport,
  exporting,
  onExport,
  pivotActive,
  onTogglePivot,
}: {
  canExport: boolean;
  exporting: boolean;
  onExport: () => void;
  pivotActive: boolean;
  onTogglePivot: () => void;
}) {
  const [menuPos, setMenuPos] = useState<{ top: number; right: number } | null>(null);
  useEffect(() => {
    if (!menuPos) return;
    const close = () => setMenuPos(null);
    const onKey = (e: KeyboardEvent) => { if (e.key === "Escape") close(); };
    // Deferred so the opening click itself doesn't immediately close it.
    const t = setTimeout(() => document.addEventListener("click", close), 0);
    document.addEventListener("keydown", onKey);
    return () => { clearTimeout(t); document.removeEventListener("click", close); document.removeEventListener("keydown", onKey); };
  }, [menuPos]);

  const itemStyle: React.CSSProperties = {
    display: "flex", alignItems: "center", gap: 8, width: "100%",
    padding: "7px 12px", border: "none", background: "transparent",
    fontSize: 13, color: "var(--color-text)", cursor: "pointer", textAlign: "left",
    fontFamily: "inherit",
  };

  return (
    <>
      <IconButton
        aria-label="Grid actions"
        title="Grid actions"
        aria-expanded={menuPos !== null}
        onClick={(e) => {
          const r = (e.currentTarget as HTMLElement).getBoundingClientRect();
          setMenuPos(menuPos ? null : { top: r.bottom + 4, right: window.innerWidth - r.right });
        }}
      >
        <MoreVertical size={15} />
      </IconButton>
      {menuPos && (
        <div
          role="menu"
          style={{
            position: "fixed", top: menuPos.top, right: menuPos.right, zIndex: 1200,
            minWidth: 160, background: "var(--color-surface)",
            border: "1px solid var(--color-border)", borderRadius: "var(--radius-button)",
            boxShadow: "var(--shadow-popover, 0 4px 16px rgba(0,0,0,0.12))", padding: 4,
          }}
        >
          {canExport && (
            <button
              type="button" role="menuitem" style={itemStyle} disabled={exporting}
              onClick={() => { onExport(); setMenuPos(null); }}
            >
              <Download size={14} />
              {exporting ? "Exporting…" : "Export"}
            </button>
          )}
          <button
            type="button" role="menuitem" style={itemStyle}
            onClick={() => { onTogglePivot(); setMenuPos(null); }}
          >
            <Rows3 size={14} />
            {pivotActive ? "Hide pivot" : "Pivot data"}
          </button>
        </div>
      )}
    </>
  );
}

// ── Dimensional Planning Grid (multi-dim) ─────────────────────────────────────

// Cartesian product of member arrays, each item = one member per dim.
function crossProduct(dims: DimInfo[]): DimMember[][] {
  if (dims.length === 0) return [[]];
  const [first, ...rest] = dims;
  const restProduct = crossProduct(rest);
  return first.members.flatMap(m => restProduct.map(combo => [m, ...combo]));
}

// Composite cell key for a metric + ordered member codes.
function cellKey(metricId: string, codes: string[]): string {
  return codes.length === 0 ? metricId : `${metricId}:${codes.join(":")}`;
}

// Unique key for a combo (independent of metric).
function comboKey(members: DimMember[]): string {
  return members.map(m => m.code).join(":");
}

// ── Dimension hierarchy helpers ───────────────────────────────────────────────

interface MemberTreeNode {
  member: DimMember;
  children: MemberTreeNode[];
  level: number;
  leafCount: number; // 1 for true leaves, sum of descendant leaves otherwise
  colCount: number;  // 1 for true leaves, else 1 (this node's own trailing Total column) + sum of children's colCount
}

function buildMemberTree(members: DimMember[]): MemberTreeNode[] {
  const byCode = new Map<string, MemberTreeNode>();
  for (const m of members) {
    byCode.set(m.code, { member: m, children: [], level: 0, leafCount: 1, colCount: 1 });
  }
  const roots: MemberTreeNode[] = [];
  for (const node of byCode.values()) {
    const parent = node.member.parent_code ? byCode.get(node.member.parent_code) : undefined;
    if (parent) parent.children.push(node);
    else roots.push(node);
  }
  function init(nodes: MemberTreeNode[], level: number) {
    nodes.sort((a, b) => a.member.code.localeCompare(b.member.code));
    for (const n of nodes) {
      n.level = level;
      init(n.children, level + 1);
      n.leafCount = n.children.length === 0 ? 1 : n.children.reduce((s, c) => s + c.leafCount, 0);
      n.colCount = n.children.length === 0 ? 1 : 1 + n.children.reduce((s, c) => s + c.colCount, 0);
    }
  }
  roots.sort((a, b) => a.member.code.localeCompare(b.member.code));
  init(roots, 0);
  return roots;
}

function memberTreeDepth(nodes: MemberTreeNode[]): number {
  let d = 0;
  function walk(n: MemberTreeNode) { if (n.level > d) d = n.level; n.children.forEach(walk); }
  nodes.forEach(walk);
  return d;
}

// Depth-first flat list for hierarchical row rendering.
function flattenTree(nodes: MemberTreeNode[]): { member: DimMember; level: number; isLeaf: boolean }[] {
  const out: { member: DimMember; level: number; isLeaf: boolean }[] = [];
  function walk(n: MemberTreeNode) {
    out.push({ member: n.member, level: n.level, isLeaf: n.children.length === 0 });
    n.children.forEach(walk);
  }
  nodes.forEach(walk);
  return out;
}

// Depth-first list of only the leaf members (for column data combos).
function treeLeaves(nodes: MemberTreeNode[]): DimMember[] {
  const out: DimMember[] = [];
  function walk(n: MemberTreeNode) {
    if (n.children.length === 0) out.push(n.member);
    else n.children.forEach(walk);
  }
  nodes.forEach(walk);
  return out;
}


// Depth-first column-axis members for DATA columns: every leaf, plus a
// trailing entry for each group node's own aggregated Total column (visited
// after its children, matching this file's existing grand-Total-row
// convention, which is also trailing). Unlike treeLeaves, group/parent nodes
// ARE included — they get a real, read-only, aggregated data column instead
// of being header-only.
function colAxisEntries(nodes: MemberTreeNode[]): DimMember[] {
  const out: DimMember[] = [];
  function walk(n: MemberTreeNode) {
    if (n.children.length === 0) { out.push(n.member); return; }
    n.children.forEach(walk);
    out.push(n.member);
  }
  nodes.forEach(walk);
  return out;
}

// Multi-level column header structure. Each level is an array of cells with
// colSpan = number of columns spanned below (leaf descendants + one trailing
// Total column per intermediate group), rowSpan = 1 unless the cell is
// itself a leaf/Total column and needs to span down to the last header row.
// Every non-leaf node also gets a synthetic isTotal cell one level below its
// own, labeling that node's trailing aggregate column from colAxisEntries.
type ColHeaderCell = { member: DimMember; colSpan: number; rowSpan: number; isTotal?: boolean };
function buildColHeaderLevels(roots: MemberTreeNode[], depth: number): ColHeaderCell[][] {
  const levels: ColHeaderCell[][] = Array.from({ length: depth + 1 }, () => []);
  function walk(n: MemberTreeNode) {
    const leaf = n.children.length === 0;
    levels[n.level].push({
      member: n.member,
      colSpan: n.colCount,
      rowSpan: leaf ? depth - n.level + 1 : 1,
    });
    if (!leaf) {
      n.children.forEach(walk);
      levels[n.level + 1].push({
        member: n.member,
        colSpan: 1,
        rowSpan: depth - (n.level + 1) + 1,
        isTotal: true,
      });
    }
  }
  roots.forEach(walk);
  return levels;
}

// ── Cross-dimension rollup helpers ────────────────────────────────────────────
//
// A whole dimension can declare another as its parent_dimension_id (e.g. Cabinet
// is a child of Department), or derive its members from another dimension's
// member property (e.g. Region <- Employee.properties.region). These helpers
// let a formula/grid reference a metric that lives at a different, related
// dimension grain and get the correctly aggregated value — walking through
// any number of intermediate dimension levels, any number of the metric's own
// dimensions at once, and same-dimension hierarchy (e.g. months collapsed to
// years) too.

function dimensionById(grid: GridData, id: string): DimInfo | undefined {
  return (grid.all_dimensions ?? grid.dimensions).find(d => d.id === id);
}

// Recursively folds in every ancestor dimension's own members (walking
// parent_dimension_id, capped depth 10, same cap as dimensionChainTo) into
// dim's own member list — so a pivot axis placed on a dimension with a
// declared cross-dimension parent (e.g. employees -> cost_centers) can be
// grouped/subtotaled by that parent exactly like a same-dimension hierarchy
// (e.g. months -> years), with zero changes to buildMemberTree/isAggNode/
// resolveCell: a member's parent_code already resolves correctly across
// dimensions (the backend's parent_member_id join doesn't care which
// dimension the parent belongs to), so once the parent's own members are
// present in the combined list, the existing parent_code matching nests them
// automatically.
function withAncestorMembers(grid: GridData, dim: DimInfo, depth = 0): DimMember[] {
  if (depth > 10 || !dim.parent_dimension_id) return dim.members;
  const parent = dimensionById(grid, dim.parent_dimension_id);
  if (!parent) return dim.members;
  return [...dim.members, ...withAncestorMembers(grid, parent, depth + 1)];
}

// Chain of dimensions from `dim` up to (and including) `ancestorId`, e.g.
// [Cabinet, Department, Region] when walking from Cabinet to Region.
// undefined if ancestorId isn't actually an ancestor of dim (capped at depth 10).
function dimensionChainTo(grid: GridData, dim: DimInfo, ancestorId: string, depth = 0): DimInfo[] | undefined {
  if (depth > 10) return undefined;
  if (dim.id === ancestorId) return [dim];
  if (!dim.parent_dimension_id) return undefined;
  const parent = dimensionById(grid, dim.parent_dimension_id);
  if (!parent) return undefined;
  const rest = dimensionChainTo(grid, parent, ancestorId, depth + 1);
  return rest ? [dim, ...rest] : undefined;
}

// Every member of chain[0]'s dimension descending (through every level of the
// chain) from `ancestorMember`, which belongs to chain[chain.length-1].
function descendantsInChain(chain: DimInfo[], ancestorMember: DimMember): DimMember[] {
  let current = [ancestorMember];
  for (let i = chain.length - 2; i >= 0; i--) {
    const codes = new Set(current.map(m => m.code));
    current = chain[i].members.filter(m => m.parent_code !== undefined && codes.has(m.parent_code));
  }
  return current;
}

// Walk `startMember` (in chain[0]'s dimension) up through parent_code at each
// level to find its ancestor's code in chain[chain.length-1]'s dimension.
function ancestorCodeInChain(chain: DimInfo[], startMember: DimMember): string | undefined {
  let current = startMember;
  for (let i = 0; i < chain.length - 1; i++) {
    if (!current.parent_code) return undefined;
    const next = chain[i + 1].members.find(m => m.code === current.parent_code);
    if (!next) return undefined;
    current = next;
  }
  return current.code;
}

// Every leaf-descendant code of `code` within `dim` (same-dimension hierarchy
// via parent_code) — or just [code] when it has no children (it's already a
// leaf). Lets a grid display a hierarchical dimension collapsed to a parent
// level (e.g. months -> years) and still get real summed values instead of a
// blank/direct lookup against a member no fact is ever entered against.
function leafDescendantCodes(dim: DimInfo, code: string): string[] {
  const children = dim.members.filter(m => m.parent_code === code);
  if (children.length === 0) return [code];
  return children.flatMap(c => leafDescendantCodes(dim, c.code));
}

// Every leaf code in `dim` — used when a referenced metric has a dimension
// that isn't sliced by the referencing grid/chart at all (see
// resolveCrossDimensionValue): rather than fail the whole resolution, that
// axis is aggregated over in full, exactly like a plain SUM with no filter.
function allLeafCodes(dim: DimInfo): string[] {
  const hasChildren = new Set(dim.members.filter(m => m.parent_code !== undefined).map(m => m.parent_code as string));
  return dim.members.filter(m => !hasChildren.has(m.code)).map(m => m.code);
}

// Resolves the set of metricDim member codes that correspond to gridDim's
// current member `gridMember` — one axis of resolveCrossDimensionValue's
// per-metric-dimension resolution. undefined when metricDim has no
// relationship to gridDim at all (exact, structural, or property-based).
function resolveAxisCodes(
  grid: GridData,
  metricDim: DimInfo,
  gridDim: DimInfo,
  gridMember: DimMember,
): string[] | undefined {
  // Exact match: same dimension, possibly viewed at a different hierarchy
  // level than the metric's native data (e.g. a grid showing `months`
  // collapsed to year parents while salary is entered per leaf month).
  if (metricDim.id === gridDim.id) {
    return leafDescendantCodes(metricDim, gridMember.code);
  }

  // Structural relation (parent_dimension_id), either direction, any number
  // of levels — e.g. employees -> cost_centers.
  const childChain = dimensionChainTo(grid, metricDim, gridDim.id);
  if (childChain && childChain.length > 1) {
    return descendantsInChain(childChain, gridMember).map(mm => mm.code);
  }
  const parentChain = dimensionChainTo(grid, gridDim, metricDim.id);
  if (parentChain && parentChain.length > 1) {
    const ancestorCode = ancestorCodeInChain(parentChain, gridMember);
    return ancestorCode === undefined ? [] : [ancestorCode];
  }

  // Property relation — gridDim's members are a grouping of metricDim's
  // members by a property value (e.g. regions <- employees.properties.region).
  if (gridDim.source_dimension_id === metricDim.id && gridDim.source_property) {
    const prop = gridDim.source_property;
    return metricDim.members
      .filter(mm => mm.properties?.[prop] === gridMember.code)
      .map(mm => mm.code);
  }

  return undefined;
}

// Resolves metric `m`'s value for the current grid's `combo`, rolling up
// (or broadcasting down) each of m's own dimensions against whichever of the
// current grid's `dims` it relates to (exactly, structurally, or by
// property) — e.g. a target grid dimensioned by [departments] correctly
// sums a metric dimensioned by the child [staff] under the current
// department (structural roll-up), or repeats a metric dimensioned by the
// parent [departments] across every [staff] row (structural broadcast).
// Metrics assigned to more than one dimension (e.g. salary: [employees,
// months]) are resolved by combining each dimension's resolved code-set via
// Cartesian product before summing — e.g. a cost_centers x months grid rolls
// employees up (structural) while holding months fixed (exact match).
//
// A dimension of m's that has NO relationship to any of the current grid's
// dims at all (the grid doesn't slice by it in any way — e.g. a `months`
// axis on a metric referenced from a plain department-only grid) is
// aggregated over in full (allLeafCodes) rather than aborting the whole
// resolution — "months" isn't a filter here, it just isn't broken out, so
// referencing the metric should sum across all of it, the same way it would
// if the grid had no dimensions at all. Returns undefined only when m has no
// dimensions of its own to relate (falls through to resolveCell/getVal's own
// broadcast-total fallback, grid.totals[m.id]).
function resolveCrossDimensionValue(
  grid: GridData,
  m: Metric,
  dims: DimInfo[],
  combo: DimMember[],
): number | undefined {
  const ownDimIds = m.dimension_ids ?? [];
  if (ownDimIds.length === 0) return undefined;

  const axisCodeSets: string[][] = [];
  for (const ownDimId of ownDimIds) {
    const ownDim = dimensionById(grid, ownDimId);
    if (!ownDim) return undefined;

    let resolved: string[] | undefined;
    for (let i = 0; i < dims.length; i++) {
      const gridMember = combo[i];
      if (!gridMember) continue;
      resolved = resolveAxisCodes(grid, ownDim, dims[i], gridMember);
      if (resolved !== undefined) break;
    }
    // No relationship to any of the current grid's dims — not a failure,
    // just an axis this view doesn't slice by. Sum over all of it.
    if (resolved === undefined) resolved = allLeafCodes(ownDim);
    axisCodeSets.push(resolved);
  }

  // Cartesian product across all resolved axes, in m.dimension_ids order —
  // matching how the backend keys grid.cells for this metric. An empty axis
  // (e.g. a cost center with no employees yet) collapses the whole product
  // to zero cells, which correctly sums to 0 rather than falling through to
  // an unrelated broadcast total.
  let combos: string[][] = [[]];
  for (const codes of axisCodeSets) {
    const next: string[][] = [];
    for (const prefix of combos) {
      for (const code of codes) next.push([...prefix, code]);
    }
    combos = next;
  }

  const dvals = combos.map(codes => grid.cells[cellKey(m.id, codes)] ?? 0);
  switch (m.agg_rule) {
    case "average": return dvals.length ? dvals.reduce((a, b) => a + b, 0) / dvals.length : 0;
    case "count":   return dvals.filter(v => v !== 0).length;
    default:        return dvals.reduce((a, b) => a + b, 0);
  }
}

const METRICS_ID = "__metrics__";

export function PlanningGrid({ ctx, gridDefId, defaultView, syncContext, title, selectorsPosition }: { ctx: DemoContext; gridDefId?: string; defaultView?: GridDefaultView; syncContext?: boolean; title?: string; selectorsPosition?: SelectorsPosition }) {
  const qc = useQueryClient();

  // Two-query split for server-side context scoping. The META query is cheap
  // (dimensions/metrics/access, no cells) and tells us the context selectors;
  // the CELLS query then fetches only the pinned slice. This is what keeps a
  // 500-member grid sub-second — the old single whole-model read returned the
  // entire cross-product (~120k cells, ~9s) on every 2s poll.
  const [pivotContextReady, setPivotContextReady] = useState(false);
  const { data: gridMeta, isLoading: metaLoading, error: metaError } = useQuery({
    queryKey: ["grid-meta", ctx.revision_id, gridDefId ?? "all"],
    queryFn: () => api.getGrid(ctx.revision_id, gridDefId, { metaOnly: true }),
    refetchInterval: 30000, // structure changes rarely
  });

  const writeback = useMutation({
    mutationFn: (vars: { metric_id: string; dim_codes: Record<string, string>; value: number }) =>
      api.writeback({ ...vars, model_id: ctx.model_id, revision_id: ctx.revision_id }),
    onSuccess: () => { qc.invalidateQueries({ queryKey: ["grid-cells"] }); qc.invalidateQueries({ queryKey: ["grid-meta"] }); },
  });

  // Export requires a specific grid_def_id (the raw fact_input values are
  // resolved per-grid, see gridExport in internal/gateway) — unavailable
  // when this panel is rendered for the whole-model "all dimensions" view.
  const exportGrid = useMutation({
    mutationFn: () => api.exportGrid(gridDefId!, ctx.revision_id, "xlsx"),
    onSuccess: ({ blob, filename }) => downloadBlob(blob, filename),
  });

  const [editing, setEditing]   = useState<Record<string, string>>({});
  // Right-click on an input cell opens its change history (enterprise).
  const [historyCell, setHistoryCell] = useState<CellRef | null>(null);
  const [showPivot, setShowPivot] = useState(false);

  // Pivot config: ordered arrays of item IDs (dim ID or METRICS_ID)
  const [pivotRows, setPivotRows]       = useState<string[]>([METRICS_ID]);
  const [pivotCols, setPivotCols]       = useState<string[]>([]);
  const [pivotContext, setPivotContext] = useState<string[]>([]);
  const [filterSel, setFilterSel]       = useState<Record<string, string>>({});
  // Which metric is selected when METRICS_ID is in the context zone
  const [contextMetric, setContextMetric] = useState<string>("");

  // DnD state
  const [draggedId, setDraggedId]       = useState<string | null>(null);
  const [dragOverZone, setDragOverZone] = useState<string | null>(null);

  // The pinned context sent to the server: each context dim's current
  // selection (or default). METRICS_ID isn't a real dimension, so it's
  // excluded. Empty → server returns the whole model (small grids with no
  // context dims keep working unchanged).
  const scope = useMemo(() => {
    const s: Record<string, string> = {};
    for (const dimId of pivotContext) {
      if (dimId === METRICS_ID) continue;
      const code = filterSel[dimId];
      if (code) s[dimId] = code;
    }
    return s;
  }, [pivotContext, filterSel]);
  const scopeKey = JSON.stringify(scope);

  const { data: cellsData, error: cellsError } = useQuery({
    queryKey: ["grid-cells", ctx.revision_id, gridDefId ?? "all", scopeKey],
    queryFn: () => api.getGrid(ctx.revision_id, gridDefId, { scope }),
    enabled: pivotContextReady,
    refetchInterval: 2000,
    placeholderData: (prev) => prev, // keep last slice visible while a new scope loads
  });

  // Merge cheap metadata with the scoped cells so everything downstream still
  // reads a single `grid` object (dimensions/metrics from meta, cells/totals
  // from the scoped query).
  const grid = useMemo(() => {
    if (!gridMeta) return undefined;
    return { ...gridMeta, cells: cellsData?.cells ?? {}, totals: cellsData?.totals ?? {} };
  }, [gridMeta, cellsData]);
  const isLoading = metaLoading;
  const error = metaError ?? cellsError;

  // Initialise pivot config when dims first arrive or grid changes
  const dimsKey = (grid?.dimensions ?? []).map(d => d.id).join(",");
  useEffect(() => {
    const dims = grid?.dimensions ?? [];
    if (!dims.length) return;
    const allKnown = new Set([...pivotRows, ...pivotCols, ...pivotContext]);
    const needsInit = dims.some(d => !allKnown.has(d.id)) || allKnown.size === 0;
    if (!needsInit) return;

    const validId = (id: string) => id === METRICS_ID || dims.some(d => d.id === id);
    if (defaultView) {
      // eslint-disable-next-line react-hooks/set-state-in-effect -- initialize pivot from developer-set default
      setPivotRows(defaultView.rows.filter(validId));
      setPivotCols(defaultView.cols.filter(validId));
      setPivotContext(defaultView.context.filter(validId));
      const baseSel = Object.fromEntries(dims.map(d => [d.id, defaultLeafCode(d) ?? ""]));
      setFilterSel({ ...baseSel, ...(defaultView.filter_sel ?? {}) });
    } else {
      // Fallback: Metrics in Rows, first dim in Cols, rest in Context
      setPivotRows([METRICS_ID]);
      setPivotCols(dims.length > 0 ? [dims[0].id] : []);
      setPivotContext(dims.slice(1).map(d => d.id));
      // A context dim's default selection must be a leaf — d.members[0] is
      // sorted by code, and for a same-dimension hierarchy (e.g. months
      // with year parents) a parent code like "2026" sorts before its own
      // leaf "2026-01", which would make comboIsAgg/resolveCell treat every
      // combo as touching an agg node instead of showing real data.
      setFilterSel(Object.fromEntries(dims.map(d => [d.id, defaultLeafCode(d) ?? ""])));
    }
    setContextMetric(cm => cm || (grid?.metrics[0]?.id ?? ""));
    setPivotContextReady(true);
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [dimsKey]);

  // Must be called unconditionally on every render (rules of hooks), so
  // this sits before the early-return guards below. Uses raw
  // grid?.dimensions rather than the fully-processed post-guard `dims`/
  // `ctxDims` (display-level filtering, ancestor augmentation) — a
  // deliberate simplification that only affects the seed value in the
  // edge case of a context dim with a non-null display_level; actual
  // rendering/cell-lookup always uses the real ctxDims + filterSel below.
  const ctxDimsForSync = (grid?.dimensions ?? []).filter(d => pivotContext.includes(d.id));
  const setFilterValue = useWidgetContextSync(ctxDimsForSync, !!syncContext, filterSel, setFilterSel);
  const ownsSelector = useSelectorOwnership(!!syncContext);
  // Click-to-select: a member label on a row/column axis pushes that member
  // into the dashboard-wide context, so every synced selector for the same
  // dimension (KPIs, charts, other grids' filters) follows — "click Germany,
  // see Germany everywhere". Null when this grid is not synced.
  // Works on any dashboard, independent of this grid's own sync flag: the
  // click is the user's explicit choice (see ChartWidget for the same rule).
  const pushSync = useSyncSetter(true);
  const selPos: SelectorsPosition = selectorsPosition ?? "top";
  const selVertical = selPos === "left" || selPos === "right";
  const pickProps = (dimId: string | undefined, dimName: string | undefined, code: string, label: string) =>
    pushSync && dimId
      ? { className: "mvx-header-cell--pickable", title: `Set ${dimName ?? "context"} to ${label} for synced widgets`, onClick: () => pushSync(dimId, code) }
      : {};
  const pickComboProps = (combo: { code: string; label: string }[], axisDims: { id: string; name: string }[]) =>
    pushSync && combo.length > 0
      ? {
          className: "mvx-header-cell--pickable",
          title: `Set ${combo.map((m, i) => `${axisDims[i]?.name ?? "context"} to ${m.label}`).join(", ")} for synced widgets`,
          onClick: () => combo.forEach((m, i) => { if (axisDims[i]) pushSync(axisDims[i].id, m.code); }),
        }
      : {};

  if (isLoading) return <LoadingState label="Loading grid…" />;
  if (error) return <ErrorState message={(error as Error).message} />;
  if (!grid) return null;
  // Alias to a non-optional const so nested closures see the narrowed type.
  const g = grid;

  // Backend has already filtered hidden members/metrics and marked readonly ones.
  // Just unpack — no client-side ID lookup needed.
  const dims = (g.dimensions ?? []).map(dim => {
    const level = dim.display_level;
    if (level == null) return dim; // null = all levels, no filtering
    const lvl: number = level;    // narrow away null for closures below
    const tree = buildMemberTree(dim.members);
    let filtered: DimMember[];
    if (lvl === -1) {
      filtered = treeLeaves(tree);
    } else {
      const out: DimMember[] = [];
      function collectLevel(nodes: MemberTreeNode[]) {
        for (const n of nodes) {
          if (n.level === lvl) out.push(n.member);
          else if (n.level < lvl) collectLevel(n.children);
        }
      }
      collectLevel(tree);
      filtered = out.length > 0 ? out : dim.members;
    }
    // Return a dim with only the filtered members and no parent_code relationships
    // (the selected level is treated as flat — no further hierarchy).
    return { ...dim, display_level: null, members: filtered.map(m => ({ ...m, parent_code: undefined })) };
  }).map(dim => ({ ...dim, members: withAncestorMembers(g, dim) }));
  const visibleMetrics = g.metrics ?? [];
  const metricsInRows    = pivotRows.includes(METRICS_ID);
  const metricsInContext = pivotContext.includes(METRICS_ID);
  const rowDims  = dims.filter(d => pivotRows.includes(d.id));
  const colDims  = dims.filter(d => pivotCols.includes(d.id));
  const ctxDims  = dims.filter(d => pivotContext.includes(d.id));
  // When metrics are in the context zone, only show the one selected metric in the grid
  const gridMetrics = metricsInContext
    ? visibleMetrics.filter(m => m.id === (contextMetric || visibleMetrics[0]?.id))
    : visibleMetrics;

  // ── Hierarchy-aware dimension trees ────────────────────────────────────────
  // Trees are built only for single-dimension row/col zones; multi-dim zones
  // keep the existing flat crossProduct behaviour.
  const colTree  = colDims.length === 1 ? buildMemberTree(colDims[0].members)  : null;
  const rowTree  = rowDims.length === 1 ? buildMemberTree(rowDims[0].members)  : null;
  const colDepth = colTree ? memberTreeDepth(colTree) : 0;
  const rowDepth = rowTree ? memberTreeDepth(rowTree) : 0;
  const hasColHierarchy = colDepth > 0;
  const hasRowHierarchy = rowDepth > 0;

  // effectiveColCombos: leaves + one trailing group-Total column per parent
  // when hierarchy, full crossProduct otherwise. A parent/group node gets a
  // real, read-only, aggregated data column (via colAxisEntries) rather than
  // being header-only — comboIsAgg/resolveCell already aggregate it correctly
  // regardless of whether the hierarchy is same-dimension or cross-dimension.
  const colAxisMembers = hasColHierarchy && colTree ? colAxisEntries(colTree) : null;
  const effectiveColCombos: DimMember[][] = colAxisMembers
    ? colAxisMembers.map(m => [m])
    : crossProduct(colDims);

  // colHeaderLevels: multi-level header structure for hierarchical column dims.
  const colHeaderLevels: ColHeaderCell[][] | null = hasColHierarchy && colTree
    ? buildColHeaderLevels(colTree, colDepth)
    : null;

  // flatRowEntries: depth-first ordered row entries for hierarchical row dims.
  const flatRowEntries = hasRowHierarchy && rowTree ? flattenTree(rowTree) : null;

  const rowCombos = crossProduct(rowDims);   // [[]] when empty → one "All" row
  const colCombos = crossProduct(colDims);   // full crossProduct for formula evaluation

  // Build full DimMember[] in dims order for a (rowCombo, colCombo) pair
  function fullCombo(rc: DimMember[], cc: DimMember[]): DimMember[] {
    const byId: Record<string, string> = {};
    rowDims.forEach((d, i) => { byId[d.id] = rc[i]?.code ?? ""; });
    colDims.forEach((d, i) => { byId[d.id] = cc[i]?.code ?? ""; });
    // Default context selection must land on a leaf member — d.members[0]
    // is sorted by code, and for a same-dimension hierarchy (e.g. months
    // with year parents) a parent code like "2026" sorts before its own
    // leaf "2026-01", which would make every combo look like it touches an
    // agg node (isAggNode below) and always resolve via aggregation instead
    // of a direct leaf lookup.
    ctxDims.forEach(d => { byId[d.id] = filterSel[d.id] ?? defaultLeafCode(d) ?? ""; });
    return dims.map(d => {
      const code = byId[d.id] ?? "";
      return d.members.find(m => m.code === code) ?? { id: "", code, label: code };
    });
  }

  function getKey(metricId: string, fc: DimMember[]): string {
    return dims.length === 0 ? metricId : cellKey(metricId, fc.map(m => m.code));
  }

  // Returns true if the member is a parent aggregation node in its dimension
  function isAggNode(dim: DimInfo, member: DimMember): boolean {
    return dim.members.some(m => m.parent_code === member.code);
  }

  // Recursively resolves a cell value for any metric (INPUT or CALC) at any combo.
  // For parent dimension members, aggregates children using the metric's agg_rule.
  // For leaf combos, reads grid.cells directly — the server now populates it
  // with calc-metric values (from runtime.calc_result) exactly like it always
  // has for input values, so no client-side formula evaluation is needed here.
  // All-metrics lookup carries dimension_ids (see backend grid() comment on
  // all_metrics) — g.metrics entries don't, so resolveCrossDimensionValue
  // (which needs a metric's own dims to roll up) must look up here.
  function fullMetric(metricId: string): Metric | undefined {
    return (g.all_metrics ?? g.metrics).find(m => m.id === metricId);
  }

  function resolveCell(metricId: string, fc: DimMember[], depth = 0): number | undefined {
    if (depth > 10) return 0;
    if (dims.length === 0) return g.totals[metricId] ?? 0;
    const metric = g.metrics.find(m => m.id === metricId);
    const aggRule = metric?.agg_rule ?? "sum";

    // 'formula' and 'rate' cannot be derived from children at all. The first
    // is this metric's own expression re-evaluated against aggregated inputs;
    // the second is one metric's total divided by another's. Neither is a
    // combination of the values below it, and the browser has no formula
    // evaluator to work one out — that was removed on purpose when grid values
    // converged on server-computed ones.
    //
    // The switch below has no case for either, so they fell to `default` and
    // were summed: a Sales metric set to Formula showed 92 + 200 = 292 where
    // its own rule says 13.5 x 54 = 729. That is the same silent-sum the
    // server's rollup.combineAgg used to do, still living in the client.
    //
    // So take the server's answer and never combine. Only the grand total is
    // published for these rules, which is the aggregate this grid actually
    // renders; an intermediate rollup in a deeper hierarchy has no
    // server-side value to read and is left blank rather than invented.
    if (aggRule === "formula" || aggRule === "rate") {
      const serverCell = g.cells[getKey(metricId, fc)];
      if (serverCell !== undefined) return serverCell;
      // g.totals is the server's aggregate over the WHOLE grid — it knows
      // nothing about the client's context selectors. The only combo it
      // truthfully answers is the one where EVERY dimension is rolled up to
      // its root: the grand total itself. Any other combo — a root rollup
      // while a context selector pins another dimension to a leaf, an
      // intermediate rollup, or a genuinely empty leaf intersection — has
      // no client-derivable value for these rules (no formula evaluator
      // here, and combining per-combo ratios is exactly the wrong math this
      // branch exists to prevent), so it renders as "—" rather than any
      // number that might be false. Two live incidents forced this shape:
      // empty leaf cells first showed the grand total (a Canada-restricted
      // user saw margin_pct 60 at a no-data intersection), and after that
      // was fixed, the World rollup column showed the all-periods total
      // under a Q1 context — "these mistakes must never happen".
      const allRoots = dims.every((dim, i) => isAggNode(dim, fc[i]) && !fc[i].parent_code);
      return allRoots ? (g.totals[metricId] ?? 0) : undefined;
    }

    for (let i = 0; i < dims.length; i++) {
      const dim = dims[i];
      const member = fc[i];
      const children = dim.members.filter(m => m.parent_code === member.code);
      if (children.length === 0) continue;
      const childVals = children.map(child => {
        const childFc = [...fc];
        childFc[i] = child;
        // The formula/rate branch above returns before this loop, so a
        // child resolving to undefined can only mean a nested formula/rate
        // — which never recurses here; ?? 0 is for the type, not a case.
        return resolveCell(metricId, childFc, depth + 1) ?? 0;
      });
      switch (aggRule) {
        case "average": return childVals.length ? childVals.reduce((a, b) => a + b, 0) / childVals.length : 0;
        case "count":   return childVals.filter(v => v !== 0).length;
        default:        return childVals.reduce((a, b) => a + b, 0);
      }
    }
    // Leaf combo — direct fact lookup first; if this grid mirrors a metric whose native
    // dims differ from its own (a rollup grid — see rollup_source_grid_id),
    // no direct cell will ever exist here, so fall back to the same
    // cross-dimension resolver formulas use.
    const direct = g.cells[getKey(metricId, fc)];
    if (direct !== undefined) return direct;
    const full = fullMetric(metricId);
    if (full) {
      const crossDim = resolveCrossDimensionValue(g, full, dims, fc);
      if (crossDim !== undefined) return crossDim;
    }
    return 0;
  }

  // Is any member in this combo a parent (agg node)?
  function comboIsAgg(fc: DimMember[]): boolean {
    return dims.some((dim, i) => isAggNode(dim, fc[i]));
  }

  function getVal(metricId: string, fc: DimMember[]): number | undefined {
    if (dims.length === 0) return g.totals[metricId];
    // Parent combos always aggregate from children — stored direct values are stale and ignored
    if (comboIsAgg(fc)) return resolveCell(metricId, fc);
    const direct = g.cells[getKey(metricId, fc)];
    if (direct !== undefined) return direct;
    const full = fullMetric(metricId);
    if (full) {
      const crossDim = resolveCrossDimensionValue(g, full, dims, fc);
      if (crossDim !== undefined) return crossDim;
    }
    return undefined;
  }

  function commitCell(metric: Metric, fc: DimMember[]) {
    const key = getKey(metric.id, fc);
    const raw = editing[key];
    if (raw === undefined) return;
    const value = parseFloat(raw.replace(/,/g, ""));
    if (!isNaN(value)) {
      const dim_codes: Record<string, string> = {};
      dims.forEach((d, i) => { dim_codes[d.id] = fc[i]?.code ?? ""; });
      writeback.mutate({ metric_id: metric.id, dim_codes, value });
    }
    setEditing(e => { const n = { ...e }; delete n[key]; return n; });
  }

  // Move an item to a zone (removes from any previous zone first)
  function moveToZone(id: string, zone: "rows" | "cols" | "context") {
    setPivotRows(p => p.filter(x => x !== id));
    setPivotCols(p => p.filter(x => x !== id));
    setPivotContext(p => p.filter(x => x !== id));
    if (zone === "rows")    setPivotRows(p => [...p, id]);
    else if (zone === "cols") setPivotCols(p => [...p, id]);
    else setPivotContext(p => [...p, id]);
  }

  // DnD handlers
  function onDragStart(e: React.DragEvent, id: string) {
    e.dataTransfer.setData("text/plain", id);
    e.dataTransfer.effectAllowed = "move";
    setDraggedId(id);
  }
  function onDragEnd() { setDraggedId(null); setDragOverZone(null); }
  function onDragOver(e: React.DragEvent, zone: string) {
    e.preventDefault(); e.dataTransfer.dropEffect = "move"; setDragOverZone(zone);
  }
  function onDragLeave() { setDragOverZone(null); }
  function onDrop(e: React.DragEvent, zone: "rows" | "cols" | "context") {
    e.preventDefault();
    const id = e.dataTransfer.getData("text/plain");
    if (id) moveToZone(id, zone);
    setDraggedId(null); setDragOverZone(null);
  }

  // Render one pivot zone (rows / cols / context)
  function renderZone(zone: "rows" | "cols" | "context", label: string, ids: string[]) {
    const isOver = dragOverZone === zone && draggedId !== null;
    return (
      <div style={{ marginBottom: 12 }}>
        <div className="mvx-prop-section">{label}</div>
        <div
          onDragOver={e => onDragOver(e, zone)}
          onDragLeave={onDragLeave}
          onDrop={e => onDrop(e, zone)}
          className={["mvx-pivot-zone", isOver ? "mvx-pivot-zone--over" : ""].filter(Boolean).join(" ")}
        >
          {ids.map(id => {
            const isMetrics = id === METRICS_ID;
            const dim = dims.find(d => d.id === id);
            const name = isMetrics ? "Metrics" : (dim?.name ?? id);
            return (
              <div
                key={id}
                draggable
                onDragStart={e => onDragStart(e, id)}
                onDragEnd={onDragEnd}
                className={["mvx-pivot-chip", draggedId === id ? "mvx-pivot-chip--dragging" : ""].filter(Boolean).join(" ")}
              >
                <GripVertical size={12} aria-hidden="true" style={{ color: "var(--color-text-subtle)", flexShrink: 0 }} />
                <span className="mvx-admin-mono" style={{
                  minWidth: 14, fontWeight: 600,
                  color: isMetrics ? "var(--color-brand-600)" : "var(--color-text-subtle)",
                }}>
                  {isMetrics ? "fx" : "≡"}
                </span>
                <span style={{ flex: 1, fontSize: 12, fontWeight: 500 }}>{name}</span>
                {zone === "context" && dim && (
                  // stopPropagation on click/mousedown: the trigger sits inside a
                  // draggable chip, and a custom <button> (unlike a native
                  // <select>) doesn't get the browser's own exemption from
                  // initiating an ancestor's HTML5 drag on mousedown.
                  <span onClick={e => e.stopPropagation()}>
                    <HierarchicalMemberSelect
                      ariaLabel={`${name} context`}
                      members={dim.members}
                      value={filterSel[id] ?? defaultLeafCode(dim) ?? ""}
                      onChange={code => setFilterValue(id, code)}
                      onMouseDown={e => e.stopPropagation()}
                    />
                  </span>
                )}
                {zone === "context" && isMetrics && (
                  <Select
                    value={(contextMetric || visibleMetrics[0]?.id) ?? ""}
                    onChange={e => { e.stopPropagation(); setContextMetric(e.target.value); }}
                    onClick={e => e.stopPropagation()}
                    aria-label="Metric context"
                  >
                    {visibleMetrics.map(m => <option key={m.id} value={m.id}>{m.label}</option>)}
                  </Select>
                )}
              </div>
            );
          })}
          {ids.length === 0 && (
            <div className="mvx-pivot-zone__empty">drop here</div>
          )}
        </div>
      </div>
    );
  }

  // Grid helpers
  const hasColDims = colDims.length > 0;
  const hasRowDims = rowDims.length > 0;
  const numMetrics = gridMetrics.length;

  function colTotal(metricId: string, cc: DimMember[]): number {
    return rowCombos.reduce((sum, rc) => sum + (getVal(metricId, fullCombo(rc, cc)) ?? 0), 0);
  }

  // Rows for "metrics in rows" mode: dim combos as group headers, metrics as sub-rows
  type RowEntry = { kind: "group"; rc: DimMember[] } | { kind: "metric"; metric: Metric; rc: DimMember[] };
  const metricRowEntries: RowEntry[] = metricsInRows
    ? (rowDims.length === 0
        ? visibleMetrics.map(m => ({ kind: "metric" as const, metric: m, rc: [] }))
        : rowCombos.flatMap(rc => [
            { kind: "group" as const, rc },
            ...visibleMetrics.map(m => ({ kind: "metric" as const, metric: m, rc })),
          ]))
    : [];

  // ── Shared cell renderers ─────────────────────────────────────────────────
  function inputCell(m: Metric, fc: DimMember[], borderLeft?: string) {
    const isParent = comboIsAgg(fc);
    const key = getKey(m.id, fc);
    const val = getVal(m.id, fc);

    // Read-only when: parent agg node, backend flagged metric/any dim member
    // as readonly, or this grid mirrors another grid's metrics via
    // cross-dimension rollup (rollup_source_grid_id) — a rolled-up total
    // (e.g. a cost center's summed salary) isn't a real fact cell, so
    // there's nothing meaningful to write back.
    const isReadOnly = isParent || !!m.readonly || fc.some(member => member.readonly) || !!g.rollup_source_grid_id;

    if (isReadOnly) {
      return (
        <td
          className={[isParent ? "mvx-cell--agg" : "mvx-cell--readonly", val == null ? "mvx-cell--empty" : ""].filter(Boolean).join(" ")}
          style={{ ...td, textAlign: "right", borderLeft }}
          onContextMenu={isParent ? undefined : (e) => openHistory(e, m, fc)}
          title={isParent ? undefined : "Right-click for history"}
        >
          {fmtMetric(m, val)}
        </td>
      );
    }

    const isActive = editing[key] !== undefined;
    const dispVal = editing[key] !== undefined
      ? editing[key]
      : (val != null ? fmtMetric(m, val) : "");
    return (
      <td style={{ ...td, textAlign: "right", padding: "4px 5px", borderLeft }} onContextMenu={(e) => openHistory(e, m, fc)} title="Right-click for history">
        <input
          value={dispVal} placeholder="—"
          className={["mvx-cell-input", isActive ? "mvx-cell-input--active" : ""].filter(Boolean).join(" ")}
          onChange={e => setEditing(p => ({ ...p, [key]: e.target.value }))}
          onBlur={() => commitCell(m, fc)}
          onKeyDown={e => e.key === "Enter" && commitCell(m, fc)}
        />
      </td>
    );
  }

  function openHistory(e: React.MouseEvent, m: Metric, fc: DimMember[]) {
    e.preventDefault();
    const dim_codes: Record<string, string> = {};
    dims.forEach((d, i) => { dim_codes[d.id] = fc[i]?.code ?? ""; });
    setHistoryCell({
      model_id: ctx.model_id, revision_id: ctx.revision_id, metric_id: m.id, metric_label: m.label ?? m.name,
      dim_codes, member_labels: fc.map(x => x.label ?? x.code),
    });
  }

  function calcCell(m: Metric, fc: DimMember[], borderLeft?: string) {
    const isParent = comboIsAgg(fc);
    return (
      <td
        className={["mvx-cell--calc", isParent ? "mvx-cell--agg" : ""].filter(Boolean).join(" ")}
        style={{ ...td, textAlign: "right", borderLeft }}
      >
        {fmtMetric(m, resolveCell(m.id, fc))}
      </td>
    );
  }

  return (
    <div style={{ display: "flex", gap: 16, alignItems: "flex-start" }}>
      <CellHistoryDrawer cell={historyCell} onClose={() => setHistoryCell(null)} />

      {/* ── Main content ─────────────────────────────────────────────────────── */}
      {/* Selector placement (widget_props.selectors_position): the toolbar
          with the title/context selectors sits along the top edge by
          default, or the bottom, or as a column on the left/right. */}
      <div style={{ flex: 1, minWidth: 0, display: "flex", flexDirection: selPos === "left" ? "row" : selPos === "right" ? "row-reverse" : "column", gap: selVertical ? 12 : 0 }}>

        <div style={{ order: selPos === "bottom" ? 2 : 0, flexShrink: 0, ...(selVertical ? { maxWidth: 260, display: "flex", flexDirection: "column" } : {}) }}>
        {/* One row: widget title (when hosted in a dashboard widget),
            context selectors, and the ··· actions — the title used to sit
            in its own header row with the menu on the next, stacking two
            mostly-empty bars (reported live). */}
        <Toolbar className="mvx-toolbar--tight">
          <ToolbarGroup>
            {title && <span style={{ fontWeight: 600, fontSize: "var(--font-size-body)", marginRight: 6 }}>{title}</span>}
            {ctxDims.filter(dim => ownsSelector(dim.id)).map(dim => (
              <div key={dim.id} style={{ display: "flex", alignItems: "center", gap: 5 }}>
                <span className="mvx-admin-muted" style={{ fontWeight: 500 }}>{dim.name}:</span>
                <HierarchicalMemberSelect
                  ariaLabel={`${dim.name} context`}
                  members={dim.members}
                  value={filterSel[dim.id] ?? defaultLeafCode(dim) ?? ""}
                  onChange={code => setFilterValue(dim.id, code)}
                />
              </div>
            ))}
            {metricsInContext && (
              <div style={{ display: "flex", alignItems: "center", gap: 5 }}>
                <span style={{ fontSize: 12, color: "var(--color-brand-600)", fontWeight: 500 }}>Metric:</span>
                <Select
                  value={(contextMetric || visibleMetrics[0]?.id) ?? ""}
                  onChange={e => setContextMetric(e.target.value)}
                  aria-label="Metric context"
                >
                  {visibleMetrics.map(m => <option key={m.id} value={m.id}>{m.label}</option>)}
                </Select>
              </div>
            )}
          </ToolbarGroup>
          <ToolbarGroup align="end">
            <GridActionsMenu
              canExport={!!gridDefId}
              exporting={exportGrid.isPending}
              onExport={() => exportGrid.mutate()}
              pivotActive={showPivot}
              onTogglePivot={() => setShowPivot(p => !p)}
            />
          </ToolbarGroup>
        </Toolbar>
        </div>

        <div style={{ flex: 1, minWidth: 0 }}>
        {/* ── Grid table ── */}
        <div style={{ overflowX: "auto" }}>

          {metricsInRows ? (
            /* Metrics in ROWS */
            <table style={{ borderCollapse: "collapse", fontSize: 13, width: "100%" }}>
              <thead>
                {hasColHierarchy && colHeaderLevels ? (
                  colHeaderLevels.map((level, li) => (
                    <tr key={li} style={{ background: "var(--color-grid-col-header-bg)", borderBottom: li === colHeaderLevels.length - 1 ? "2px solid var(--color-border)" : "1px solid var(--color-grid-col-header-border)" }}>
                      {li === 0 && (
                        <th style={{ ...th, minWidth: 180 }} rowSpan={colHeaderLevels.length}>
                          {rowDims.length > 0 ? rowDims.map(d => d.name).join(" / ") + " / Metric" : "Metric"}
                        </th>
                      )}
                      {level.map((cell, ci) => (
                        <th key={`${cell.member.code}-${cell.isTotal ? "total" : "self"}-${ci}`}
                          {...(cell.isTotal ? {} : pickProps(colDims[0]?.id, colDims[0]?.name, cell.member.code, cell.member.label))}
                          colSpan={cell.colSpan} rowSpan={cell.rowSpan}
                          style={{ ...th, textAlign: "center", minWidth: 90 * cell.colSpan,
                            borderLeft: "1px solid var(--color-grid-col-header-border)",
                            color: cell.isTotal ? "var(--color-text-quiet)" : (cell.colSpan > 1 ? "var(--color-grid-col-header-text)" : "var(--color-text-strong)"),
                            fontWeight: cell.isTotal ? 700 : (cell.colSpan > 1 ? 600 : 500),
                            fontStyle: cell.isTotal ? "italic" : undefined,
                            background: cell.colSpan > 1 ? "var(--color-grid-col-header-bg)" : "var(--color-grid-col-header-bg-soft)",
                            fontSize: cell.colSpan > 1 ? 12 : 11 }}>
                          {cell.isTotal ? "Total" : cell.member.label}
                        </th>
                      ))}
                    </tr>
                  ))
                ) : (
                  <tr style={{ background: colDims.length > 0 ? "var(--color-grid-col-header-bg)" : "var(--color-surface-faint)", borderBottom: "2px solid var(--color-border)" }}>
                    <th style={{ ...th, minWidth: 180 }}>
                      {rowDims.length > 0 ? rowDims.map(d => d.name).join(" / ") + " / Metric" : "Metric"}
                    </th>
                    {colDims.length > 0
                      ? colCombos.map(cc => (
                          <th key={comboKey(cc)} {...pickComboProps(cc, colDims)} style={{ ...th, textAlign: "right", minWidth: 90, borderLeft: "1px solid var(--color-grid-col-header-border-soft)", color: "var(--color-grid-col-header-text)" }}>
                            {cc.map(m => m.label).join(" / ")}
                          </th>
                        ))
                      : <th style={{ ...th, textAlign: "right", minWidth: 90 }}>Value</th>
                    }
                  </tr>
                )}
              </thead>
              <tbody>
                {metricRowEntries.map((entry, ei) => {
                  if (entry.kind === "group") {
                    const numDataCols = hasColHierarchy ? effectiveColCombos.length : (colDims.length > 0 ? colCombos.length : 1);
                    return (
                      <tr key={`grp-${comboKey(entry.rc)}`}
                        style={{ background: "var(--color-surface-muted)", borderTop: ei > 0 ? "2px solid var(--color-border)" : undefined }}>
                        <td colSpan={numDataCols + 1}
                          style={{ ...td, fontWeight: 700, color: "var(--color-text-strong)", paddingLeft: 10 }}>
                          {entry.rc.map(m => m.label).join(" / ")}
                        </td>
                      </tr>
                    );
                  }
                  const { metric: m, rc } = entry;
                  const cols = hasColHierarchy ? effectiveColCombos : (colDims.length > 0 ? colCombos : [[]]);
                  return (
                    <tr key={`${comboKey(rc)}-${m.id}`}
                      style={{ borderBottom: "1px solid var(--color-surface-muted)", background: !m.is_input ? "var(--color-grid-row-alt-bg)" : undefined }}>
                      <td style={{
                        ...td, paddingLeft: rowDims.length > 0 ? 22 : 12,
                        fontWeight: m.is_input ? 400 : 700,
                        color: m.is_input ? "var(--color-text-strong)" : "var(--color-grid-calc-text)",
                      }}>
                        {m.label}
                        {!m.is_input && <span style={{ fontSize: 10, color: "var(--color-disabled)", marginLeft: 6 }}>CALC</span>}
                      </td>
                      {cols.map(cc => {
                        const fc = fullCombo(rc, cc);
                        return m.is_input
                          ? <React.Fragment key={comboKey(cc)}>{inputCell(m, fc)}</React.Fragment>
                          : <React.Fragment key={comboKey(cc)}>{calcCell(m, fc)}</React.Fragment>;
                      })}
                    </tr>
                  );
                })}
              </tbody>
            </table>

          ) : (
            /* Metrics in COLS */
            <table style={{ borderCollapse: "collapse", fontSize: 13, width: "100%",
              minWidth: Math.max(500, effectiveColCombos.length * numMetrics * 86 + (hasRowDims ? 160 : 80)) }}>
              <thead>
                {hasColHierarchy && colHeaderLevels ? (
                  colHeaderLevels.map((level, li) => (
                    <tr key={li} style={{ background: "var(--color-grid-col-header-bg)", borderBottom: li === colHeaderLevels.length - 1 ? "2px solid var(--color-border)" : "1px solid var(--color-grid-col-header-border)" }}>
                      {li === 0 && (
                        <th style={{ ...th, minWidth: 160 }} rowSpan={colHeaderLevels.length + 1}>
                          {hasRowDims ? rowDims.map(d => d.name).join(" / ") : ""}
                        </th>
                      )}
                      {level.map((cell, ci) => (
                        <th key={`${cell.member.code}-${cell.isTotal ? "total" : "self"}-${ci}`}
                          {...(cell.isTotal ? {} : pickProps(colDims[0]?.id, colDims[0]?.name, cell.member.code, cell.member.label))}
                          colSpan={cell.colSpan * numMetrics} rowSpan={cell.rowSpan}
                          style={{ ...th, textAlign: "center",
                            minWidth: 82 * cell.colSpan * numMetrics,
                            borderLeft: ci > 0 ? "1px solid var(--color-grid-col-header-border)" : undefined,
                            color: cell.isTotal ? "var(--color-text-quiet)" : (cell.colSpan > 1 ? "var(--color-grid-col-header-text)" : "var(--color-text-strong)"),
                            fontWeight: cell.isTotal ? 700 : (cell.colSpan > 1 ? 600 : 500),
                            fontStyle: cell.isTotal ? "italic" : undefined,
                            background: cell.colSpan > 1 ? "var(--color-grid-col-header-bg)" : "var(--color-grid-col-header-bg-soft)",
                            fontSize: cell.colSpan > 1 ? 12 : 11 }}>
                          {cell.isTotal ? "Total" : cell.member.label}
                        </th>
                      ))}
                    </tr>
                  ))
                ) : hasColDims ? (
                  <tr style={{ background: "var(--color-grid-col-header-bg)", borderBottom: "1px solid var(--color-grid-col-header-border)" }}>
                    <th style={{ ...th, minWidth: 160 }} rowSpan={2}>
                      {hasRowDims ? rowDims.map(d => d.name).join(" / ") : ""}
                    </th>
                    {colCombos.map((cc, ci) => (
                      <th key={comboKey(cc)} colSpan={numMetrics} {...pickComboProps(cc, colDims)}
                        style={{ ...th, textAlign: "center", borderLeft: ci > 0 ? "1px solid var(--color-grid-col-header-border)" : undefined, color: "var(--color-grid-col-header-text)", fontSize: 12 }}>
                        {cc.map(m => m.label).join(" / ")}
                      </th>
                    ))}
                  </tr>
                ) : null}
                <tr style={{ background: "var(--color-surface-faint)", borderBottom: "2px solid var(--color-border)" }}>
                  {!hasColDims && !hasColHierarchy && (
                    <th style={{ ...th, minWidth: 160 }}>
                      {hasRowDims ? rowDims.map(d => d.name).join(" / ") : ""}
                    </th>
                  )}
                  {effectiveColCombos.flatMap((cc, ci) =>
                    gridMetrics.map(m => (
                      <th key={`${comboKey(cc)}-${m.id}`}
                        style={{
                          ...th, textAlign: "right", minWidth: 82,
                          borderLeft: hasColDims && ci > 0 && gridMetrics[0]?.id === m.id ? "1px solid var(--color-border-muted)" : undefined,
                          color: m.is_input ? "var(--color-text-strong)" : "var(--color-grid-calc-text)",
                        }}>
                        {metricsInContext ? " " : m.label}
                        {!metricsInContext && <div style={{ fontSize: 10, fontWeight: 400, color: "var(--color-disabled)" }}>{m.is_input ? "INPUT" : "CALC"}</div>}
                      </th>
                    ))
                  )}
                </tr>
              </thead>
              <tbody>
                {(hasRowHierarchy && flatRowEntries
                  ? flatRowEntries.map(({ member, level, isLeaf }) => {
                      const rc = [member];
                      return (
                        <tr key={member.code}
                          style={{ borderBottom: "1px solid var(--color-surface-muted)",
                            background: isLeaf ? undefined : "var(--color-surface-subtle)" }}>
                          <td {...pickProps(rowDims[0]?.id, rowDims[0]?.name, member.code, member.label)} style={{ ...td,
                            paddingLeft: 12 + level * 20,
                            fontWeight: isLeaf ? 400 : 700,
                            color: "var(--color-text-strong)" }}>
                            {level > 0 && <span style={{ color: "var(--color-border-muted)", marginRight: 4, fontSize: 11 }}>└</span>}
                            {member.label}
                          </td>
                          {effectiveColCombos.flatMap((cc, ci) => {
                            const fc = fullCombo(rc, cc);
                            const bl = hasColDims && ci > 0 ? "1px solid var(--color-border)" : undefined;
                            return gridMetrics.map(m =>
                              m.is_input
                                ? <React.Fragment key={`${comboKey(cc)}-${m.id}`}>{inputCell(m, fc, ci > 0 && gridMetrics[0]?.id === m.id ? bl : undefined)}</React.Fragment>
                                : <React.Fragment key={`${comboKey(cc)}-${m.id}`}>{calcCell(m, fc, ci > 0 && gridMetrics[0]?.id === m.id ? bl : undefined)}</React.Fragment>
                            );
                          })}
                        </tr>
                      );
                    })
                  : rowCombos.map((rc, ri) => (
                      <tr key={comboKey(rc) || `row-${ri}`}
                        style={{ borderBottom: "1px solid var(--color-surface-muted)", background: ri % 2 === 1 ? "var(--color-grid-row-alt-bg)" : undefined }}>
                        <td {...pickComboProps(rc, rowDims)} style={{ ...td, fontWeight: 600, color: rc.length === 0 ? "var(--color-disabled)" : "var(--color-text-strong)" }}>
                          {rc.length === 0 ? "All" : rc.map(m => m.label).join(" / ")}
                        </td>
                        {effectiveColCombos.flatMap((cc, ci) => {
                          const fc = fullCombo(rc, cc);
                          const bl = hasColDims && ci > 0 ? "1px solid var(--color-border)" : undefined;
                          return gridMetrics.map(m =>
                            m.is_input
                              ? <React.Fragment key={`${comboKey(cc)}-${m.id}`}>{inputCell(m, fc, ci > 0 && gridMetrics[0]?.id === m.id ? bl : undefined)}</React.Fragment>
                              : <React.Fragment key={`${comboKey(cc)}-${m.id}`}>{calcCell(m, fc, ci > 0 && gridMetrics[0]?.id === m.id ? bl : undefined)}</React.Fragment>
                          );
                        })}
                      </tr>
                    ))
                )}
                {!hasRowHierarchy && rowCombos.length > 1 && (
                  <tr style={{ borderTop: "2px solid var(--color-border)", background: "var(--color-surface-faint)" }}>
                    <td style={{ ...td, fontWeight: 700 }}>Total</td>
                    {effectiveColCombos.flatMap((cc, ci) =>
                      gridMetrics.map(m => {
                        const bl = hasColDims && ci > 0 && gridMetrics[0]?.id === m.id ? "1px solid var(--color-border)" : undefined;
                        return (
                          <td key={`tot-${comboKey(cc)}-${m.id}`}
                            style={{ ...td, textAlign: "right", fontWeight: 700, color: m.is_input ? "var(--color-text-strong)" : "var(--color-grid-calc-text)", borderLeft: bl }}>
                            {fmtMetric(m, colTotal(m.id, cc))}
                          </td>
                        );
                      })
                    )}
                  </tr>
                )}
              </tbody>
            </table>
          )}
        </div>

        <div className="mvx-grid-legend" aria-label="Cell legend">
          {gridMetrics.some(m => m.is_input) && <span><span className="mvx-grid-legend__swatch mvx-grid-legend__swatch--input" />Editable</span>}
          {gridMetrics.some(m => !m.is_input) && <span><span className="mvx-grid-legend__swatch mvx-grid-legend__swatch--calc" />Calculated</span>}
          <span><span className="mvx-grid-legend__swatch mvx-grid-legend__swatch--readonly" />Read-only / total</span>
          {pushSync && <span>Click a row or column label to select it in synced widgets</span>}
        </div>
        {writeback.isPending && <p className="mvx-grid-status mvx-grid-status--saving">Saving…</p>}
        {writeback.isError && <p className="mvx-grid-status mvx-grid-status--error">Save failed: {(writeback.error as Error).message}</p>}
        </div>

      </div>

      {/* ── Pivot Panel ────────────────────────────────────────────────────── */}
      {showPivot && (
        <PropertyPanel
          title="Pivot data"
          subtitle="Drag items between zones to change the layout."
          className="mvx-property-panel--floating"
          actions={
            <IconButton aria-label="Close pivot panel" title="Close" size={26} onClick={() => setShowPivot(false)}>
              <XIcon size={14} />
            </IconButton>
          }
        >
          {renderZone("rows", "Rows", pivotRows)}
          {renderZone("cols", "Columns", pivotCols)}
          {renderZone("context", "Context selectors", pivotContext)}
        </PropertyPanel>
      )}
    </div>
  );
}

const th: React.CSSProperties = { padding: "10px 12px", textAlign: "left", fontWeight: 600, fontSize: 12, color: "var(--color-text-strong)", whiteSpace: "nowrap" };
const td: React.CSSProperties = { padding: "8px 12px", color: "var(--color-text)" };

function fmtMetric(m: Metric, n: number | null | undefined): string {
  if (n == null) return "—";
  const d = m.format_decimals ?? 0;
  switch (m.format) {
    case "percentage":
      return n.toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d }) + "%";
    case "boolean":
      return n ? "Yes" : "No";
    case "text":
      return String(n);
    case "currency": {
      const sym = m.format_currency || "$";
      return sym + n.toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d });
    }
    default:
      return n.toLocaleString("en-US", { minimumFractionDigits: d, maximumFractionDigits: d });
  }
}
