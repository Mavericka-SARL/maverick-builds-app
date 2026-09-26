import { useState, useRef, useEffect, useReducer, useMemo } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { ArrowLeft, X, Copy, Trash2 } from "lucide-react";
import { api, type WidgetProps, type SelectorsPosition, type WidgetBackground, type GridChartConfig, type DashboardWidget, type DashboardDef, type DemoContext, type GridDef, type FormDef, type AutomationRule, type IntegrationDef, type Metric, type DevDimension, type GridDefaultView } from "../../api/client";
import { ChartWidgetEditor } from "../dashboard/ChartWidgetEditor";
import { chartConfigFromDraft, isChartConfigComplete } from "../dashboard/chartTypes";
import { DashboardWidgetGrid } from "../business/DashboardWidgets";
import { defaultLeafCode, groupWidgetsIntoRows } from "../dashboardLayout";
import { Toolbar, ToolbarGroup, Button, SegmentedControl, FilterChip, StatusBadge, IconButton, Field, Select, TextInput, Textarea, NumberInput, Checkbox, PropertyPanel, ConfirmDialog, UnsavedChangesBar, useUnsavedGuard, RichText } from "../../ui";

// ── Dashboard widget palette / sizing ───────────────────────────────────────

const WIDGET_TYPE_COLORS: Record<string, { bg: string; text: string }> = {
  grid:               { bg: "var(--color-diagram-input-bg)", text: "var(--color-widget-grid-text)" },
  chart:              { bg: "var(--color-widget-chart-bg)", text: "var(--color-widget-chart-text)" },
  form:               { bg: "var(--color-widget-form-bg)", text: "var(--color-widget-form-text)" },
  automation_button:  { bg: "var(--color-diagram-calc-bg)", text: "var(--color-live)" },
  integration_button: { bg: "var(--color-widget-integration-bg)", text: "var(--color-widget-integration-text)" },
  text:               { bg: "var(--color-surface-muted)", text: "var(--color-text-strong)" },
  metric_kpi:         { bg: "var(--color-widget-metric-bg)", text: "var(--color-widget-metric-text)" },
  import:             { bg: "var(--color-widget-import-bg)", text: "var(--color-widget-import-text)" },
};
/** A dashboard image is kept inline as a data URL, so it travels with the
 *  dashboard through revision copies, exports and imports. The limit and
 *  the accepted types mirror internal/imagedata on the server. */
const MAX_IMAGE_BYTES = 512 * 1024;

function readImageAsDataURL(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    if (file.size > MAX_IMAGE_BYTES) {
      reject(new Error(`The image must be at most ${MAX_IMAGE_BYTES / 1024} KB; this one is ${Math.round(file.size / 1024)} KB.`));
      return;
    }
    const r = new FileReader();
    r.onerror = () => reject(new Error("That file could not be read."));
    r.onload = () => resolve(String(r.result));
    r.readAsDataURL(file);
  });
}


const PALETTE_TYPES = [
  { type: "grid",               label: "Grid" },
  { type: "chart",              label: "Chart" },
  { type: "form",               label: "Form" },
  { type: "metric_kpi",         label: "Metric KPI" },
  { type: "automation_button",  label: "Trigger" },
  { type: "integration_button", label: "Integration" },
  { type: "text",               label: "Text" },
  { type: "image",              label: "Image" },
  { type: "import",             label: "Import" },
];

type WidgetSizes = { minW: number; minH: number; defW: number; defH: number };
const WIDGET_SIZES: Record<string, WidgetSizes> = {
  grid:               { minW: 600, minH: 300, defW: 800, defH: 400 },
  chart:              { minW: 360, minH: 240, defW: 600, defH: 360 },
  form:               { minW: 360, minH: 300, defW: 500, defH: 400 },
  text:               { minW: 240, minH: 100, defW: 300, defH: 120 },
  automation_button:  { minW: 40, minH: 20, defW: 200, defH: 60 },
  integration_button: { minW: 40, minH: 20, defW: 200, defH: 60 },
  metric_kpi:         { minW: 220, minH: 120, defW: 260, defH: 140 },
  import:             { minW: 320, minH: 280, defW: 420, defH: 360 },
};
const FALLBACK_SIZES: WidgetSizes = { minW: 80, minH: 60, defW: 400, defH: 200 };
function getWidgetSizes(type: string): WidgetSizes { return WIDGET_SIZES[type] ?? FALLBACK_SIZES; }

// ── Dashboard Canvas constants ────────────────────────────────────────────────

const SNAP = 20;
const CANVAS_MIN_H = 600;
const CANVAS_MIN_W = 1200;
const HANDLE_SIZE = 10;

function snap(v: number): number { return Math.round(v / SNAP) * SNAP; }

type CanvasRect = { x: number; y: number; w: number; h: number };
function rectsOverlap(a: CanvasRect, b: CanvasRect): boolean {
  return a.x < b.x + b.w && a.x + a.w > b.x && a.y < b.y + b.h && a.y + a.h > b.y;
}
function findAutoPlace(occupied: CanvasRect[], newW: number, newH: number): { x: number; y: number } {
  for (let y = SNAP; y < 4000; y += SNAP) {
    for (let x = SNAP; x <= CANVAS_MIN_W - newW; x += SNAP) {
      const candidate: CanvasRect = { x, y, w: newW, h: newH };
      if (!occupied.some(r => rectsOverlap(candidate, r))) return { x, y };
    }
  }
  return { x: SNAP, y: SNAP };
}

type Interaction =
  | { kind: "move";   id: string; mx0: number; my0: number; ox: number; oy: number }
  | { kind: "resize"; id: string; mx0: number; my0: number; ow: number; oh: number; edge: "r" | "b" | "rb" };

export function DashboardCanvas({ dashId, dashName, revisionId, onBack }: { dashId: string; dashName: string; revisionId?: string; onBack: () => void }) {
  const qc = useQueryClient();

  const [mode, setMode] = useState<"design" | "preview">("design");
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [saveStatus, setSaveStatus] = useState<"idle" | "saving" | "saved" | "error">("idle");
  const [deleteConfirmId, setDeleteConfirmId] = useState<string | null>(null);
  const [showAdd, setShowAdd] = useState(false);
  const [newWidget, setNewWidget] = useState({ widget_type: "grid", ref_id: "", content: "" });
  const [imageError, setImageError] = useState("");
  const [propsDraft, setPropsDraft] = useState<Record<string, { ref_id?: string; content?: string; title?: string | null; show_title?: boolean; widget_props?: WidgetProps | null }>>({});

  const [newChartDraft, setNewChartDraft] = useState<Partial<GridChartConfig>>({ chart_type: "bar", context_defaults: {} });

  const interRef = useRef<Interaction | null>(null);
  const liveRef = useRef<Record<string, CanvasRect>>({});
  const draftRef = useRef<Record<string, CanvasRect>>({});
  const widgetsRef = useRef<DashboardWidget[]>([]);
  const [, forceRender] = useReducer((n: number) => n + 1, 0);

  // Single-mount global mouse handlers
  useEffect(() => {
    const onMove = (e: MouseEvent) => {
      const it = interRef.current;
      if (!it) return;
      const dx = e.clientX - it.mx0;
      const dy = e.clientY - it.my0;
      const cur = liveRef.current[it.id]
        ?? draftRef.current[it.id]
        ?? (() => {
          const w = widgetsRef.current.find(w => w.id === it.id);
          return { x: w?.pos_x ?? 0, y: w?.pos_y ?? 0, w: w?.size_w ?? 300, h: w?.size_h ?? 200 };
        })();
      const wt = widgetsRef.current.find(w => w.id === it.id)?.widget_type ?? "text";
      const { minW, minH } = getWidgetSizes(wt);
      if (it.kind === "move") {
        liveRef.current[it.id] = { ...cur, x: Math.max(0, snap(it.ox + dx)), y: Math.max(0, snap(it.oy + dy)) };
      } else {
        const nw = it.edge === "b" ? cur.w : Math.max(minW, snap(it.ow + dx));
        const nh = it.edge === "r" ? cur.h : Math.max(minH, snap(it.oh + dy));
        liveRef.current[it.id] = { ...cur, w: nw, h: nh };
      }
      forceRender();
    };
    const onUp = () => {
      const it = interRef.current;
      if (!it) return;
      const lp = liveRef.current[it.id];
      if (lp) {
        const overlaps = widgetsRef.current
          .filter(w => w.id !== it.id)
          .some(w => {
            const wr = draftRef.current[w.id] ?? { x: w.pos_x, y: w.pos_y, w: w.size_w, h: w.size_h };
            return rectsOverlap(lp, wr);
          });
        if (!overlaps) { draftRef.current[it.id] = lp; }
        // if overlapping: lp is discarded, widget snaps back to its draft/server position
      }
      delete liveRef.current[it.id];
      interRef.current = null;
      forceRender();
    };
    window.addEventListener("mousemove", onMove);
    window.addEventListener("mouseup", onUp);
    return () => { window.removeEventListener("mousemove", onMove); window.removeEventListener("mouseup", onUp); };
  }, []);

  // Shared by the Alt+Arrow path and the resize handles' own arrow keys, so
  // both apply the same minimum sizes and overlap rule as a mouse drag.
  const resizeBy = (widgetID: string, dw: number, dh: number) => {
    const w = widgetsRef.current.find(ww => ww.id === widgetID);
    if (!w) return;
    const cur = getRect(w);
    const { minW, minH } = getWidgetSizes(w.widget_type);
    const next = { ...cur, w: Math.max(minW, cur.w + dw), h: Math.max(minH, cur.h + dh) };
    const clashes = widgetsRef.current
      .filter(other => other.id !== widgetID)
      .some(other => rectsOverlap(next, getRect(other)));
    if (clashes) return;
    liveRef.current[widgetID] = next;
    draftRef.current[widgetID] = next;
    forceRender();
  };

  // Held in a ref so the window listener below keeps a stable dependency
  // list — the same reason widgetsRef/draftRef/liveRef exist in this file.
  const resizeByRef = useRef(resizeBy);
  resizeByRef.current = resizeBy;

  // Keyboard movement for selected widget
  useEffect(() => {
    if (mode !== "design") return;
    const onKey = (e: KeyboardEvent) => {
      const sid = selectedId;
      if (!sid) return;
      if (e.key === "Escape") { setSelectedId(null); return; }
      if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(e.key)) return;
      const w = widgetsRef.current.find(ww => ww.id === sid);
      if (!w) return;
      const step = e.shiftKey ? SNAP * 5 : SNAP;

      // Alt+Arrow resizes, mirroring what the drag handles do with a mouse —
      // previously resizing had no keyboard path at all, so a widget placed
      // by keyboard could never be sized by one.
      if (e.altKey) {
        resizeByRef.current(sid,
          e.key === "ArrowLeft" ? -step : e.key === "ArrowRight" ? step : 0,
          e.key === "ArrowUp" ? -step : e.key === "ArrowDown" ? step : 0);
        e.preventDefault();
        return;
      }

      // Reuse getRect's liveRef/draftRef/legacy-default fallback chain
      // instead of re-deriving it here: the previous inline fallback
      // ({ x: w.pos_x, ... }) skipped getWidgetSizes' per-type defaulting,
      // so any widget never dragged/resized (pos_x/size_w still unset from
      // the server) produced NaN/undefined on the very first arrow-key
      // press instead of a valid rect.
      const cur = getRect(w);
      const dx = e.key === "ArrowLeft" ? -step : e.key === "ArrowRight" ? step : 0;
      const dy = e.key === "ArrowUp"   ? -step : e.key === "ArrowDown"  ? step : 0;
      const nx = Math.max(0, cur.x + dx);
      const ny = Math.max(0, cur.y + dy);
      const next = { ...cur, x: nx, y: ny };
      // Mirror onUp's overlap check (mouse-drag release) so nudging a
      // widget onto a neighbor with arrow keys is rejected the same way
      // dragging it there is, instead of silently allowing an overlap
      // that dragging alone could never produce.
      const overlaps = widgetsRef.current
        .filter(w => w.id !== sid)
        .some(w => rectsOverlap(next, getRect(w)));
      if (!overlaps) {
        liveRef.current[sid] = next;
        draftRef.current[sid] = next;
        forceRender();
      }
      e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [selectedId, mode]);

  const { data: grids = [] }       = useQuery({ queryKey: ["dev-grids", revisionId], queryFn: () => api.listGrids(revisionId) });
  const { data: forms = [] }       = useQuery({ queryKey: ["forms"], queryFn: () => api.listForms() });
  const { data: rules = [] }       = useQuery({ queryKey: ["automation-rules"], queryFn: () => api.listAutomationRules() });
  // Only a manual, enabled rule can actually run from a dashboard button.
  // Offering the rest produced buttons that looked live and failed on click:
  // an event- or schedule-triggered rule fires from its own source, and a
  // disabled one is refused by TriggerRule.
  const eligibleRules = useMemo(
    () => (rules as AutomationRule[]).filter(r => r.trigger_type === "manual" && r.enabled),
    [rules],
  );
  const { data: integrations = [] }= useQuery({ queryKey: ["dev-integrations"], queryFn: () => api.listDevIntegrations() });
  const { data: metrics = [] }     = useQuery({ queryKey: ["metrics", revisionId], queryFn: () => revisionId ? api.getMetrics(revisionId) : Promise.resolve([]) });
  const { data: kpiScopeDims = [] } = useQuery({ queryKey: ["dev-dimensions-all", revisionId], queryFn: () => api.getDevDimensions(revisionId) });
  const { data: demoCtx }          = useQuery({ queryKey: ["demo"], queryFn: api.getDemo, staleTime: 60_000 });
  const { data: devRevisions = [] } = useQuery({ queryKey: ["dev-revisions-auto"], queryFn: () => api.getDevRevisions() });
  // Preview renders the widgets in the revision being DESIGNED. The demo
  // context is the active revision's — using it made every KPI/grid/chart
  // of a dashboard in any other revision query the wrong revision: KPIs
  // showed "—" with no selectors while the design strips (revision-scoped)
  // showed them, and Design and Preview disagreed (reported live, 2026-09-11).
  const previewCtx: DemoContext | undefined = demoCtx && revisionId
    ? { ...demoCtx, revision_id: revisionId, revision: (devRevisions as { id: string; name: string }[]).find(r => r.id === revisionId)?.name ?? demoCtx.revision }
    : demoCtx;
  // Scoped to the revision being designed: without revision_id the server
  // lists the ACTIVE revision's dashboards, so a dashboard that lives in any
  // other revision was never found and the canvas opened empty while the
  // Dashboards tab (revision-scoped) showed its widgets (reported live,
  // 2026-09-10: "Sales Overview", 6 widgets, design mode blank).
  const { data: dash, isLoading }  = useQuery({
    queryKey: ["dev-dashboard-detail", dashId, revisionId],
    queryFn: async () => {
      const list = await api.listDashboards(revisionId) as DashboardDef[];
      return list.find(d => d.id === dashId) ?? null;
    },
    refetchInterval: false,
  });

  const invalidateAll = () => {
    qc.invalidateQueries({ queryKey: ["dev-dashboard-detail", dashId] });
    qc.invalidateQueries({ queryKey: ["dev-dashboards"] });
    qc.invalidateQueries({ queryKey: ["user-dashboards"] });
    qc.invalidateQueries({ queryKey: ["user-dashboard-detail"] });
  };

  const markSaved = () => {
    setSaveStatus("saved");
    setTimeout(() => setSaveStatus(s => s === "saved" ? "idle" : s), 2000);
  };

  const addWidget = useMutation({
    mutationFn: async () => {
      const existing = dash?.widgets ?? [];
      const { defW, defH } = getWidgetSizes(newWidget.widget_type);
      const occupied = existing.map(w => draftRef.current[w.id] ?? { x: w.pos_x, y: w.pos_y, w: w.size_w, h: w.size_h });
      const { x, y } = findAutoPlace(occupied, defW, defH);
      const chartCfg = newWidget.widget_type === "chart" ? chartConfigFromDraft(newChartDraft) : null;
      return api.addDashboardWidget(dashId, {
        widget_type: newWidget.widget_type,
        ref_id: newWidget.ref_id || undefined,
        content: newWidget.content || undefined,
        pos_x: x, pos_y: y, size_w: defW, size_h: defH,
        sort_order: existing.length, col_start: 1, col_span: 12,
        widget_props: chartCfg ? { chart: chartCfg } : undefined,
      });
    },
    onSuccess: () => {
      invalidateAll();
      setShowAdd(false);
      setNewWidget({ widget_type: "grid", ref_id: "", content: "" });
      setNewChartDraft({ chart_type: "bar", context_defaults: {} });
      markSaved();
    },
  });
  const removeWidget = useMutation({
    mutationFn: (widgetId: string) => api.removeDashboardWidget(dashId, widgetId),
    onSuccess: () => { invalidateAll(); setDeleteConfirmId(null); setSelectedId(null); },
  });
  const duplicateWidget = useMutation({
    mutationFn: async (widgetId: string) => {
      const w = widgetsRef.current.find(ww => ww.id === widgetId);
      if (!w) return;
      // Duplicate what's currently on screen, including any not-yet-saved
      // drag/resize/property edits — not just the last-saved server copy.
      const rect = getRect(w);
      const pd = propsDraft[w.id];
      const title = pd?.title !== undefined ? pd.title : w.title;
      const showTitle = pd?.show_title !== undefined ? pd.show_title : w.show_title;
      const existing = widgetsRef.current.map(ww => draftRef.current[ww.id] ?? { x: ww.pos_x, y: ww.pos_y, w: ww.size_w, h: ww.size_h });
      const { x, y } = findAutoPlace(existing, rect.w, rect.h);
      const created = await api.addDashboardWidget(dashId, {
        widget_type: w.widget_type,
        ref_id: (pd?.ref_id !== undefined ? pd.ref_id : w.ref_id) || undefined,
        content: (pd?.content !== undefined ? pd.content : w.content) || undefined,
        pos_x: x, pos_y: y, size_w: rect.w, size_h: rect.h,
        sort_order: widgetsRef.current.length, col_start: 1, col_span: 12,
        widget_props: pd?.widget_props !== undefined ? pd.widget_props : w.widget_props,
      });
      // title/show_title aren't accepted at creation (POST) — only PATCH
      // supports them — so a second call carries them over when set.
      if (created?.id && (title || showTitle)) {
        await api.updateDashboardWidget(dashId, created.id, { title: title ?? undefined, show_title: showTitle });
      }
      return created;
    },
    onSuccess: () => { invalidateAll(); markSaved(); },
  });

  const widgets = dash?.widgets ?? [];
  widgetsRef.current = widgets;

  function widgetSourceLabel(w: DashboardWidget): string {
    const pd = propsDraft[w.id];
    const refId  = pd?.ref_id  !== undefined ? pd.ref_id  : w.ref_id;
    if (w.widget_type === "text" || w.widget_type === "image") return "";
    if (w.widget_type === "grid")               return (grids as GridDef[]).find(g => g.id === refId)?.name ?? refId?.slice(0, 8) ?? "—";
    if (w.widget_type === "form")               return (forms as FormDef[]).find(f => f.id === refId)?.label ?? refId?.slice(0, 8) ?? "—";
    if (w.widget_type === "automation_button")  return (rules as AutomationRule[]).find(r => r.id === refId)?.name ?? refId?.slice(0, 8) ?? "—";
    if (w.widget_type === "integration_button") return (integrations as IntegrationDef[]).find(i => i.id === refId)?.name ?? refId?.slice(0, 8) ?? "—";
    if (w.widget_type === "metric_kpi")         return (metrics as Metric[]).find(m => m.id === refId)?.label ?? refId?.slice(0, 8) ?? "—";
    if (w.widget_type === "chart")              return (grids as GridDef[]).find(g => g.id === refId)?.name ?? refId?.slice(0, 8) ?? "—";
    if (w.widget_type === "import")             return (grids as GridDef[]).find(g => g.id === refId)?.name ?? refId?.slice(0, 8) ?? "—";
    return "—";
  }

  // Design-mode selector strip: which dimension selectors THIS widget carries
  // and whether they are shared (synced with the dashboard), its own, or
  // pinned. Preview dedupes shared selectors onto the first widget, so the
  // designer could not tell which widget had which selector (reported live,
  // 2026-09-10) — design mode shows every widget's own set, separately.
  let sharedOwnerByDim: Record<string, DashboardWidget> = {};
  function widgetSelectorInfo(w: DashboardWidget): { dims: string[]; mode: "shared" | "own" | "pinned" | "total" } | null {
    const pd = propsDraft[w.id];
    const refId = pd?.ref_id !== undefined ? pd.ref_id : w.ref_id;
    const wp: WidgetProps = { ...(w.widget_props ?? {}), ...(pd?.widget_props ?? {}) };
    const dimName = (id: string) => (kpiScopeDims as DevDimension[]).find(d => d.id === id)?.name ?? id.slice(0, 8);
    const synced = wp.sync_context !== false;
    if (w.widget_type === "grid") {
      const g = (grids as GridDef[]).find(x => x.id === refId);
      if (!g) return null;
      // Same default PlanningGrid applies: first dimension on columns, the
      // rest as context selectors, unless the widget saved its own layout.
      const ctxIds = wp.default_view?.context ?? g.dimension_ids.slice(1);
      return { dims: ctxIds.map(dimName), mode: synced ? "shared" : "own" };
    }
    if (w.widget_type === "chart") {
      const g = (grids as GridDef[]).find(x => x.id === refId);
      if (!g) return null;
      const plotted = wp.chart?.dimension_id;
      return { dims: g.dimension_ids.filter(id => id !== plotted).map(dimName), mode: synced ? "shared" : "own" };
    }
    if (w.widget_type === "metric_kpi") {
      const m = (metrics as Metric[]).find(x => x.id === refId);
      if (!m) return null;
      const kmode = wp.kpi_context_mode ?? (wp.kpi_scope ? "pin" : synced ? "sync" : "total");
      if (kmode === "pin" && wp.kpi_scope) return { dims: [`${dimName(wp.kpi_scope.dimension_id)} = ${wp.kpi_scope.member_code}`], mode: "pinned" };
      if (kmode === "total") return { dims: [], mode: "total" };
      return { dims: (m.dimension_ids ?? []).map(dimName), mode: "shared" };
    }
    return null;
  }

  function addWidgetDisabled(): boolean {
    if (newWidget.widget_type === "text" || newWidget.widget_type === "image") return !newWidget.content;
    if (newWidget.widget_type === "chart") {
      if (!newWidget.ref_id) return true;
      return chartConfigFromDraft(newChartDraft) === null;
    }
    return !newWidget.ref_id;
  }

  function getRect(w: DashboardWidget): CanvasRect {
    const lp = liveRef.current[w.id];
    if (lp) return lp;
    const dp = draftRef.current[w.id];
    if (dp) return dp;
    // Legacy widgets created before the free canvas have no pos/size — fall
    // back to the type defaults so one old widget can't collapse the canvas.
    const sizes = getWidgetSizes(w.widget_type);
    return {
      x: Number.isFinite(w.pos_x) ? w.pos_x : 0,
      y: Number.isFinite(w.pos_y) ? w.pos_y : 0,
      w: Number.isFinite(w.size_w) && w.size_w > 0 ? w.size_w : sizes.defW,
      h: Number.isFinite(w.size_h) && w.size_h > 0 ? w.size_h : sizes.defH,
    };
  }

  async function saveLayout() {
    const layoutDirty = draftRef.current;
    const propsDirty  = propsDraft;
    const allIds = new Set([...Object.keys(layoutDirty), ...Object.keys(propsDirty)]);
    if (allIds.size === 0) return;
    setSaveStatus("saving");
    try {
      await Promise.all([...allIds].map(id => {
        const orig   = widgetsRef.current.find(ww => ww.id === id);
        if (!orig) return Promise.resolve();
        const layout = layoutDirty[id];
        const props  = propsDirty[id];
        return api.updateDashboardWidget(dashId, id, {
          pos_x:       layout?.x ?? orig.pos_x,
          pos_y:       layout?.y ?? orig.pos_y,
          size_w:      layout?.w ?? orig.size_w,
          size_h:      layout?.h ?? orig.size_h,
          content:     props?.content     !== undefined ? props.content     : (orig.content  ?? undefined),
          ref_id:      props?.ref_id      !== undefined ? props.ref_id      : (orig.ref_id   ?? undefined),
          title:       props?.title       !== undefined ? props.title       : orig.title,
          show_title:  props?.show_title  !== undefined ? props.show_title  : orig.show_title,
          widget_props: (() => {
            if (props?.widget_props === undefined) return orig.widget_props;
            // Don't persist an incomplete chart config — keep the previously saved one.
            if (orig.widget_type === "chart" && !isChartConfigComplete(props.widget_props?.chart ?? {})) {
              return orig.widget_props;
            }
            return props.widget_props;
          })(),
        });
      }));
      draftRef.current = {};
      setPropsDraft({});
      markSaved();
      invalidateAll();
    } catch {
      setSaveStatus("error");
    }
  }

  const selectedWidget = selectedId ? (widgets.find(w => w.id === selectedId) ?? null) : null;
  const COLORS = WIDGET_TYPE_COLORS;

  // Fetch the resolved grid data for the selected grid widget so DEFAULT VIEW shows
  // the same dimensions business users will see (getGrid redirects to the active revision).
  const selectedGridRefId = selectedWidget?.widget_type === "grid" ? (selectedWidget.ref_id ?? "") : "";
  const { data: selectedGridData } = useQuery({
    queryKey: ["grid-preview", previewCtx?.revision_id, selectedGridRefId],
    queryFn: () => api.getGrid(previewCtx!.revision_id, selectedGridRefId),
    enabled: !!previewCtx?.revision_id && !!selectedGridRefId,
    staleTime: 60_000,
  });

  // Computed before the loading early-return: useUnsavedGuard is a hook, so
  // it has to run on every render, not only once the canvas has data.
  const isDirty = Object.keys(draftRef.current).length > 0 || Object.keys(propsDraft).length > 0;
  // Leaving the designer — via Back, a reload, or closing the tab — used to
  // drop an unsaved layout silently, even though the UnsavedChangesBar was
  // sitting right there saying it wasn't saved.
  const { guard: guardUnsaved, guardElement } = useUnsavedGuard(isDirty);

  if (isLoading) return <p style={{ color: "var(--color-text-quiet)" }}>Loading canvas…</p>;

  const isInteracting = interRef.current !== null;
  const draggedId = interRef.current?.id ?? null;

  // Collision detection: which widget ids overlap the currently dragged widget
  const collidingWith = new Set<string>();
  if (draggedId) {
    const dragRect = getRect(widgets.find(w => w.id === draggedId)!);
    for (const w of widgets) {
      if (w.id !== draggedId && rectsOverlap(dragRect, getRect(w))) collidingWith.add(w.id);
    }
  }
  const dragOverlaps = collidingWith.size > 0;

  // Canvas sizing
  const canvasH    = Math.max(CANVAS_MIN_H, ...widgets.map(w => getRect(w).y + getRect(w).h + 80));
  const canvasMinW = Math.max(CANVAS_MIN_W, ...widgets.map(w => getRect(w).x + getRect(w).w + 40));

  return (
    <div style={{ userSelect: isInteracting ? "none" : undefined }}>

      {/* ── Toolbar ── */}
      <Toolbar className="mvx-toolbar--spaced">
        <ToolbarGroup>
          <Button size="sm" variant="ghost" leadingIcon={<ArrowLeft size={14} />} onClick={() => guardUnsaved(onBack)}>
            Dashboards
          </Button>
          <span style={{ fontWeight: 700, fontSize: 17 }}>{dashName}</span>
          <span className="mvx-admin-muted">{widgets.length} widget{widgets.length !== 1 ? "s" : ""}</span>

          <SegmentedControl
            aria-label="Builder mode"
            segments={[
              { id: "design", label: "Design" },
              { id: "preview", label: "Preview" },
            ]}
            value={mode}
            onChange={(m) => {
              setMode(m);
              if (m === "preview") { setSelectedId(null); setShowAdd(false); }
            }}
          />

          {mode === "design" && (
            <>
              <div style={{ width: 1, height: 20, background: "var(--color-border)", flexShrink: 0 }} />
              {PALETTE_TYPES.map(({ type, label }) => {
                const isActive = showAdd && newWidget.widget_type === type;
                return (
                  <FilterChip
                    key={type}
                    active={isActive}
                    onClick={() => {
                      if (isActive) { setShowAdd(false); return; }
                      setNewWidget({ widget_type: type, ref_id: "", content: "" });
                      setShowAdd(true);
                    }}
                  >
                    {label}
                  </FilterChip>
                );
              })}
            </>
          )}
        </ToolbarGroup>
      </Toolbar>

      {/* ── Add Widget Panel ── */}
      {showAdd && mode === "design" && (() => {
        const palLabel  = PALETTE_TYPES.find(p => p.type === newWidget.widget_type)?.label ?? newWidget.widget_type;
        return (
        <div className="mvx-panel" style={{ padding: 20, marginBottom: 16, background: "var(--color-surface-subtle)" }}>
          <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 14 }}>
            <StatusBadge tone="brand">{palLabel}</StatusBadge>
            <span style={{ fontSize: 13, fontWeight: 600, flex: 1 }}>Configure &amp; place</span>
            <IconButton aria-label="Close" title="Close" size={26} onClick={() => setShowAdd(false)}>
              <X size={14} />
            </IconButton>
          </div>
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(220px, 1fr))", gap: 14, marginBottom: 14 }}>
            {newWidget.widget_type === "grid" && (
              <Field label="Grid">
                <Select value={newWidget.ref_id} onChange={e => setNewWidget(w => ({ ...w, ref_id: e.target.value }))}>
                  <option value="">— select —</option>
                  {(grids as GridDef[]).map(g => <option key={g.id} value={g.id}>{g.name}</option>)}
                </Select>
              </Field>
            )}
            {newWidget.widget_type === "chart" && (
              <div style={{ gridColumn: "1 / -1" }}>
                <Field label="Grid (source)" style={{ marginBottom: 10 }}>
                  <Select
                    value={newWidget.ref_id}
                    onChange={e => {
                      setNewWidget(w => ({ ...w, ref_id: e.target.value }));
                      setNewChartDraft({ chart_type: "bar", context_defaults: {} });
                    }}
                  >
                    <option value="">— select —</option>
                    {(grids as GridDef[]).map(g => <option key={g.id} value={g.id}>{g.name}</option>)}
                  </Select>
                </Field>
                {newWidget.ref_id && previewCtx && (
                  <ChartWidgetEditor
                    gridDefId={newWidget.ref_id}
                    draft={newChartDraft}
                    ctx={previewCtx as DemoContext}
                    onChange={patch => setNewChartDraft(prev => ({ ...prev, ...patch }))}
                  />
                )}
              </div>
            )}
            {newWidget.widget_type === "form" && (
              <Field label="Form">
                <Select value={newWidget.ref_id} onChange={e => setNewWidget(w => ({ ...w, ref_id: e.target.value }))}>
                  <option value="">— select —</option>
                  {(forms as FormDef[]).map(f => <option key={f.id} value={f.id}>{f.label || f.name}</option>)}
                </Select>
              </Field>
            )}
            {newWidget.widget_type === "metric_kpi" && (
              <Field label="Metric">
                <Select value={newWidget.ref_id} onChange={e => setNewWidget(w => ({ ...w, ref_id: e.target.value }))}>
                  <option value="">— select —</option>
                  {(metrics as Metric[]).map(m => <option key={m.id} value={m.id}>{m.label || m.name}</option>)}
                </Select>
              </Field>
            )}
            {newWidget.widget_type === "automation_button" && (
              <Field label="Trigger">
                <Select value={newWidget.ref_id} onChange={e => setNewWidget(w => ({ ...w, ref_id: e.target.value }))}>
                  <option value="">— select —</option>
                  {eligibleRules.map(r => <option key={r.id} value={r.id}>{r.name}</option>)}
                </Select>
                {eligibleRules.length === 0 && (
                  <p className="mvx-admin-muted" style={{ fontSize: 12, margin: "4px 0 0" }}>
                    No manual, enabled automation rules yet — a button can only run those.
                  </p>
                )}
              </Field>
            )}
            {newWidget.widget_type === "integration_button" && (
              <Field label="Integration">
                <Select value={newWidget.ref_id} onChange={e => setNewWidget(w => ({ ...w, ref_id: e.target.value }))}>
                  <option value="">— select —</option>
                  {(integrations as IntegrationDef[]).map(i => <option key={i.id} value={i.id}>{i.name}</option>)}
                </Select>
              </Field>
            )}
            {newWidget.widget_type === "text" && (
              <Field label="Content" description="Markdown: # heading, **bold**, - bullets, [label](https://example.com)">
                <Textarea value={newWidget.content} onChange={e => setNewWidget(w => ({ ...w, content: e.target.value }))} rows={4} style={{ width: "100%", resize: "vertical" }} placeholder="Text content…" />
              </Field>
            )}
            {newWidget.widget_type === "image" && (
              <Field label="Image file" description={`PNG, JPEG, SVG, WebP or GIF, up to ${MAX_IMAGE_BYTES / 1024} KB. The picture is stored with the dashboard.`}>
                <input
                  type="file"
                  accept="image/*"
                  aria-label="Image file"
                  onChange={async e => {
                    const file = e.target.files?.[0];
                    if (!file) return;
                    try {
                      setImageError("");
                      const data = await readImageAsDataURL(file);
                      setNewWidget(w => ({ ...w, content: data }));
                    } catch (err) { setImageError((err as Error).message); }
                  }}
                />
                {imageError && <div className="mvx-admin-error">{imageError}</div>}
                {newWidget.content?.startsWith("data:") && <img src={newWidget.content} alt="" style={{ maxWidth: "100%", maxHeight: 120, marginTop: 8, display: "block" }} />}
              </Field>
            )}
            {newWidget.widget_type === "import" && (
              <Field label="Grid">
                <Select value={newWidget.ref_id} onChange={e => setNewWidget(w => ({ ...w, ref_id: e.target.value }))}>
                  <option value="">— select —</option>
                  {(grids as GridDef[]).map(g => <option key={g.id} value={g.id}>{g.name}</option>)}
                </Select>
              </Field>
            )}
          </div>
          <Button variant="primary" disabled={addWidgetDisabled()} loading={addWidget.isPending} loadingLabel="Adding…"
            onClick={() => addWidget.mutate()}>
            Add widget
          </Button>
          {addWidget.isError && <p className="mvx-admin-error" style={{ marginTop: 8 }}>{(addWidget.error as Error).message}</p>}
        </div>
        );
      })()}

      {/* ── Unsaved layout changes bar ── */}
      {guardElement}
      {isDirty && mode === "design" && (
        <UnsavedChangesBar
          onSave={saveLayout}
          onCancel={() => { draftRef.current = {}; setPropsDraft({}); setSaveStatus("idle"); }}
          saving={saveStatus === "saving"}
          saveLabel="Save"
          error={saveStatus === "error" ? "Layout save failed. Please try again." : undefined}
        />
      )}

      {/* ── Main: canvas + properties panel ── */}
      <div style={{ display: "flex", gap: 16, alignItems: "flex-start" }}>

        {/* Canvas / Preview */}
        <div style={{ flex: 1, minWidth: 0, overflowX: mode === "preview" ? undefined : "auto" }}>
          {mode === "preview" ? (
            <div style={{ display: "flex", flexDirection: "column", gap: 16 }}>
              {widgets.length === 0 && (
                <p style={{ color: "var(--color-disabled)", fontSize: 13 }}>No widgets on this dashboard yet.</p>
              )}
              {!previewCtx && widgets.length > 0 && (
                <p style={{ color: "var(--color-text-quiet)", fontSize: 13 }}>Loading preview…</p>
              )}
              {previewCtx && (
                // Same component, same row-grouping, same per-widget sizing
                // as the real business-facing page — so Preview shows
                // exactly what a business user will see, not an
                // approximation with its own border/clipping rules. Widgets
                // reflect any not-yet-saved property-panel edits (pd), same
                // as the design canvas itself.
                <DashboardWidgetGrid
                  widgets={widgets.map(w => {
                    const pd = propsDraft[w.id];
                    if (!pd) return w;
                    return {
                      ...w,
                      content:      pd.content      !== undefined ? pd.content      : w.content,
                      ref_id:       pd.ref_id       !== undefined ? pd.ref_id       : w.ref_id,
                      title:        pd.title        !== undefined ? pd.title        : w.title,
                      show_title:   pd.show_title   !== undefined ? pd.show_title   : w.show_title,
                      widget_props: pd.widget_props !== undefined ? pd.widget_props : w.widget_props,
                    };
                  })}
                  ctx={previewCtx as DemoContext}
                  dashboardId={dashId}
                />
              )}
            </div>
          ) : (
          <div
            aria-label="Dashboard design canvas"
            style={{
              position: "relative",
              width: canvasMinW,
              height: canvasH,
              borderRadius: 10,
              border: "1px solid var(--color-border)",
              background: "var(--color-surface-subtle)",
              backgroundImage: "radial-gradient(circle, var(--color-border-muted) 1px, transparent 1px)",
              backgroundSize: `${SNAP}px ${SNAP}px`,
              cursor: isInteracting ? (interRef.current?.kind === "move" ? "grabbing" : "crosshair") : "default",
            }}
            onClick={e => { if (e.target === e.currentTarget) setSelectedId(null); }}
          >
            {widgets.length === 0 && mode === "design" && (
              <div style={{ position: "absolute", inset: 0, display: "flex", flexDirection: "column", alignItems: "center", justifyContent: "center", color: "var(--color-disabled)", gap: 6 }}>
                <div style={{ fontSize: 28 }}>⬚</div>
                <div style={{ fontSize: 13 }}>No widgets yet.</div>
                <div style={{ fontSize: 12 }}>Add a Grid, KPI, Form, or Text widget to start designing this dashboard.</div>
              </div>
            )}

            {/* Shared-selector ownership, computed the way Preview renders
                it: the first synced widget in layout order (rows top to
                bottom, left to right) draws a shared dimension's selector,
                every later synced widget follows it. The strips say so —
                listing the same selector on every card read as if each
                widget had its own (reported live). */}
            {(() => { sharedOwnerByDim = {}; for (const row of groupWidgetsIntoRows(widgets)) for (const rw of row) { const inf = widgetSelectorInfo(rw); if (inf?.mode !== "shared") continue; for (const d of inf.dims) if (!sharedOwnerByDim[d]) sharedOwnerByDim[d] = rw; } return null; })()}
            {widgets.map(w => {
              const r = getRect(w);
              const isActive   = interRef.current?.id === w.id;
              const isSelected = selectedId === w.id;
              const isDragged  = draggedId === w.id;
              const isColliding = collidingWith.has(w.id) || (isDragged && dragOverlaps);
              const colors = COLORS[w.widget_type] ?? COLORS.text;
              const isButtonWidget = w.widget_type === "automation_button" || w.widget_type === "integration_button";

              return (
                <div
                  key={w.id}
                  tabIndex={mode === "design" ? 0 : undefined}
                  aria-label={`${w.widget_type.replace(/_/g, " ")} widget: ${widgetSourceLabel(w)}`}
                  className={mode === "design" ? "mvx-canvas-widget" : undefined}
                  onClick={e => { if (mode === "design") { e.stopPropagation(); setSelectedId(w.id); } }}
                  // Tabbing to a widget selects it, so the arrow-key move and
                  // Alt+Arrow resize below actually reach it — the container
                  // was focusable already, but only a mouse click ever set
                  // selectedId, leaving the keyboard path unreachable.
                  onFocus={() => { if (mode === "design") setSelectedId(w.id); }}
                  style={{
                    position: "absolute",
                    left: r.x, top: r.y, width: r.w, height: r.h,
                    background: "var(--color-surface)",
                    border: isColliding  ? "2px solid var(--color-danger-accent)"
                           : isSelected  ? "2px solid var(--color-brand-600)"
                           : "1px solid var(--color-border)",
                    borderRadius: 8,
                    boxShadow: isActive   ? "0 4px 16px rgba(79,70,229,0.15)"
                              : isSelected ? "0 2px 8px rgba(79,70,229,0.10)"
                              : "0 1px 4px rgba(0,0,0,0.06)",
                    display: "flex",
                    flexDirection: "column",
                    overflow: "hidden",
                    // No `outline: none` here: it suppressed the focus ring on
                    // a focusable element, so keyboard users had no idea which
                    // widget they were on. .mvx-canvas-widget styles
                    // :focus-visible instead.
                  }}
                >
                  {/* Design header (drag to move) — hidden for button widgets */}
                  {mode === "design" && !isButtonWidget && (
                    <div
                      onMouseDown={e => {
                        e.preventDefault();
                        setSelectedId(w.id);
                        const dr = getRect(w);
                        interRef.current = { kind: "move", id: w.id, mx0: e.clientX, my0: e.clientY, ox: dr.x, oy: dr.y };
                        forceRender();
                      }}
                      style={{
                        display: "flex", alignItems: "center", gap: 6,
                        padding: "7px 10px",
                        borderBottom: "1px solid var(--color-border-muted)",
                        cursor: isActive && interRef.current?.kind === "move" ? "grabbing" : "grab",
                        background: isSelected ? "var(--color-widget-integration-bg)" : "var(--color-surface-muted)",
                        flexShrink: 0,
                      }}
                    >
                      <span style={{ color: "var(--color-disabled)", fontSize: 14 }}>⠿</span>
                      <span style={{ fontSize: 10, fontWeight: 700, borderRadius: 4, padding: "2px 6px", background: colors.bg, color: colors.text }}>
                        {w.widget_type.replace(/_/g, " ").toUpperCase()}
                      </span>
                      {widgetSourceLabel(w) && (
                        <span style={{ fontSize: 12, fontWeight: 600, color: "var(--color-text-strong)", flex: 1, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                          {widgetSourceLabel(w)}
                        </span>
                      )}
                      <IconButton
                        aria-label="Delete widget"
                        title="Delete widget"
                        size={24}
                        style={{ flexShrink: 0 }}
                        onMouseDown={e => e.stopPropagation()}
                        onClick={e => { e.stopPropagation(); setDeleteConfirmId(w.id); }}>
                        <X size={12} />
                      </IconButton>
                    </div>
                  )}

                  {/* Optional title bar preview */}
                  {!isButtonWidget && (() => {
                    const pd = propsDraft[w.id];
                    const showTitle = pd?.show_title !== undefined ? pd.show_title : w.show_title;
                    const title = pd?.title !== undefined ? pd.title : w.title;
                    return showTitle && title ? (
                      <div style={{ padding: "5px 10px", fontSize: 12, fontWeight: 600, color: "var(--color-text-strong)", background: "var(--color-surface-faint)", borderBottom: "1px solid var(--color-border)", flexShrink: 0 }}>
                        {title}
                      </div>
                    ) : null;
                  })()}

                  {/* Button widget: fills entire widget, drag via body */}
                  {isButtonWidget && (() => {
                    const pd = propsDraft[w.id];
                    const label = (pd?.content !== undefined ? pd.content : w.content) || (w.widget_type === "automation_button" ? "Run" : "Send");
                    // Literal hex, not a design token: this feeds a native
                    // <input type="color"> value below, which requires a
                    // concrete hex string — var() is not a valid value there.
                    const defaultColor = w.widget_type === "automation_button" ? "#22c55e" : "#3b82f6";
                    const buttonColor = pd?.widget_props?.button_color ?? w.widget_props?.button_color ?? defaultColor;
                    return (
                      <div
                        onMouseDown={e => {
                          e.preventDefault();
                          setSelectedId(w.id);
                          const dr = getRect(w);
                          interRef.current = { kind: "move", id: w.id, mx0: e.clientX, my0: e.clientY, ox: dr.x, oy: dr.y };
                          forceRender();
                        }}
                        style={{ flex: 1, display: "flex", position: "relative", cursor: isActive && interRef.current?.kind === "move" ? "grabbing" : "grab" }}
                      >
                        <button style={{
                          flex: 1, width: "100%", border: "none",
                          background: buttonColor, color: "var(--color-text-inverse)",
                          fontSize: 14, fontWeight: 600,
                          cursor: "inherit", pointerEvents: "none",
                          borderRadius: 0,
                        }}>
                          {label}
                        </button>
                        {isSelected && (
                          <button
                            onMouseDown={e => e.stopPropagation()}
                            onClick={e => { e.stopPropagation(); setDeleteConfirmId(w.id); }}
                            style={{
                              position: "absolute", top: 6, right: 6,
                              width: 22, height: 22, borderRadius: "50%",
                              border: "none", background: "rgba(0,0,0,0.35)",
                              color: "var(--color-text-inverse)", fontSize: 11, cursor: "pointer",
                              display: "flex", alignItems: "center", justifyContent: "center",
                            }}>
                            ✕
                          </button>
                        )}
                      </div>
                    );
                  })()}

                  {/* Body — non-button widgets */}
                  {!isButtonWidget && (
                    <div style={{ flex: 1, overflow: "auto", padding: "8px 10px", fontSize: 12, color: "var(--color-text-quiet)" }}>
                      {mode === "design" && (() => {
                        const info = widgetSelectorInfo(w);
                        if (!info) return null;
                        const badge = info.mode === "shared" ? "synced" : info.mode === "own" ? "own" : info.mode === "pinned" ? "pinned" : "total";
                        const pos = ({ ...(w.widget_props ?? {}), ...(propsDraft[w.id]?.widget_props ?? {}) } as WidgetProps).selectors_position ?? "top";
                        const tone = info.mode === "shared" ? "brand" : info.mode === "own" ? "warning" : "neutral";
                        return (
                          <div aria-label="Widget selectors" title={`Selectors (${badge}): ${info.dims.join(", ") || "none"}`} style={{ display: "flex", flexWrap: "nowrap", alignItems: "center", gap: 4, padding: "5px 10px", borderBottom: "1px solid var(--color-border-muted)", background: "var(--color-surface-faint)", fontSize: 11, overflow: "hidden", whiteSpace: "nowrap", flexShrink: 0 }}>
                            <span style={{ color: "var(--color-text-quiet)", fontWeight: 600, textTransform: "uppercase", letterSpacing: "0.04em", fontSize: 10 }}>Selectors</span>
                            <StatusBadge tone={tone}>{badge}</StatusBadge>
                            {info.dims.length === 0 && <span style={{ color: "var(--color-text-quiet)" }}>{info.mode === "total" ? "none (grand total)" : "none"}</span>}
                            {info.dims.map(d => {
                              const owner = info.mode === "shared" ? sharedOwnerByDim[d] : undefined;
                              if (owner && owner.id !== w.id) {
                                const ownerName = owner.title || widgetSourceLabel(owner) || owner.widget_type.replace(/_/g, " ");
                                return <span key={d} title={`Shared selector drawn on "${ownerName}"; this widget follows it`} style={{ color: "var(--color-text-quiet)", whiteSpace: "nowrap" }}>{d} → {ownerName}</span>;
                              }
                              return <FilterChip key={d}>{d}</FilterChip>;
                            })}
                            {info.dims.length > 0 && pos !== "top" && <span style={{ color: "var(--color-text-quiet)" }}>· {pos}</span>}
                          </div>
                        );
                      })()}
                      {w.widget_type === "image" && (() => {
                        const pd = propsDraft[w.id];
                        const content = pd?.content !== undefined ? pd.content : w.content;
                        const wp = { ...(w.widget_props ?? {}), ...(pd?.widget_props ?? {}) };
                        return content
                          ? <img src={content} alt={wp.alt ?? ""} style={{ width: "100%", height: "100%", objectFit: wp.image_fit ?? "contain" }} />
                          : <span className="mvx-admin-muted">No image chosen</span>;
                      })()}
                      {w.widget_type === "text" && (() => {
                        const pd = propsDraft[w.id];
                        const content = pd?.content !== undefined ? pd.content : w.content;
                        const wp = { ...(w.widget_props ?? {}), ...(pd?.widget_props ?? {}) };
                        return content
                          ? <RichText text={content} style={{ fontSize: wp.font_size ? `${wp.font_size}px` : 14, fontWeight: wp.font_weight ?? "normal", color: wp.color ?? "var(--color-text-strong)", fontFamily: wp.font_family ?? "sans-serif" }} />
                          : null;
                      })()}
                    </div>
                  )}

                  {/* Resize size tooltip */}
                  {isActive && interRef.current?.kind === "resize" && (
                    <div style={{ position: "absolute", bottom: HANDLE_SIZE + 2, left: 6, fontSize: 10, color: "var(--color-brand-600)", background: "color-mix(in srgb, var(--color-surface) 92%, transparent)", borderRadius: 3, padding: "1px 4px", pointerEvents: "none" }}>
                      {r.w} × {r.h}
                    </div>
                  )}

                  {/* Overlap warning */}
                  {isDragged && dragOverlaps && (
                    <div style={{ position: "absolute", top: 4, right: 36, fontSize: 10, background: "var(--color-danger-bg)", color: "var(--color-danger-accent)", borderRadius: 4, padding: "2px 6px", fontWeight: 600, pointerEvents: "none" }}>
                      Overlaps another widget
                    </div>
                  )}

                  {/* Resize handles (design only) */}
                  {mode === "design" && <>
                    <div
                      role="button"
                      tabIndex={0}
                      aria-label="Resize widget width"
                      onMouseDown={e => { e.preventDefault(); e.stopPropagation(); const dr = getRect(w); interRef.current = { kind: "resize", id: w.id, mx0: e.clientX, my0: e.clientY, ow: dr.w, oh: dr.h, edge: "r" }; forceRender(); }}
                      onFocus={() => setSelectedId(w.id)}
                      onKeyDown={e => {
                        if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(e.key)) return;
                        // Plain arrows resize while a handle has focus, so the
                        // handle does the same job for a keyboard as a drag
                        // does for a mouse. Stops here so the window-level
                        // handler doesn't also MOVE the widget.
                        e.preventDefault();
                        e.stopPropagation();
                        const step = e.shiftKey ? SNAP * 5 : SNAP;
                        resizeBy(w.id, e.key === "ArrowLeft" ? -step : e.key === "ArrowRight" ? step : 0, 0);
                      }}
                      style={{ position: "absolute", top: 0, right: 0, bottom: HANDLE_SIZE, width: HANDLE_SIZE, cursor: "ew-resize" }}
                    />
                    <div
                      role="button"
                      tabIndex={0}
                      aria-label="Resize widget height"
                      onMouseDown={e => { e.preventDefault(); e.stopPropagation(); const dr = getRect(w); interRef.current = { kind: "resize", id: w.id, mx0: e.clientX, my0: e.clientY, ow: dr.w, oh: dr.h, edge: "b" }; forceRender(); }}
                      onFocus={() => setSelectedId(w.id)}
                      onKeyDown={e => {
                        if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(e.key)) return;
                        // Plain arrows resize while a handle has focus, so the
                        // handle does the same job for a keyboard as a drag
                        // does for a mouse. Stops here so the window-level
                        // handler doesn't also MOVE the widget.
                        e.preventDefault();
                        e.stopPropagation();
                        const step = e.shiftKey ? SNAP * 5 : SNAP;
                        resizeBy(w.id, 0, e.key === "ArrowUp" ? -step : e.key === "ArrowDown" ? step : 0);
                      }}
                      style={{ position: "absolute", bottom: 0, left: 0, right: HANDLE_SIZE, height: HANDLE_SIZE, cursor: "s-resize" }}
                    />
                    <div
                      role="button"
                      tabIndex={0}
                      aria-label="Resize widget width and height"
                      onMouseDown={e => { e.preventDefault(); e.stopPropagation(); const dr = getRect(w); interRef.current = { kind: "resize", id: w.id, mx0: e.clientX, my0: e.clientY, ow: dr.w, oh: dr.h, edge: "rb" }; forceRender(); }}
                      onFocus={() => setSelectedId(w.id)}
                      onKeyDown={e => {
                        if (!["ArrowLeft", "ArrowRight", "ArrowUp", "ArrowDown"].includes(e.key)) return;
                        // Plain arrows resize while a handle has focus, so the
                        // handle does the same job for a keyboard as a drag
                        // does for a mouse. Stops here so the window-level
                        // handler doesn't also MOVE the widget.
                        e.preventDefault();
                        e.stopPropagation();
                        const step = e.shiftKey ? SNAP * 5 : SNAP;
                        resizeBy(w.id, e.key === "ArrowLeft" ? -step : e.key === "ArrowRight" ? step : 0, e.key === "ArrowUp" ? -step : e.key === "ArrowDown" ? step : 0);
                      }}
                      style={{ position: "absolute", bottom: 0, right: 0, width: HANDLE_SIZE, height: HANDLE_SIZE, cursor: "se-resize", background: isSelected ? "var(--color-brand-200)" : "var(--color-border)", borderRadius: "4px 0 6px 0" }}
                    />
                  </>}
                </div>
              );
            })}
          </div>
          )}
        </div>

        {/* ── Properties panel ── */}
        {mode === "design" && selectedWidget && (
          <PropertyPanel
            title="Widget Properties"
            className="mvx-property-panel--floating"
            footer={
              <div style={{ display: "flex", gap: 8 }}>
                <Button variant="secondary" size="sm" leadingIcon={<Copy size={13} />} style={{ flex: 1 }}
                  loading={duplicateWidget.isPending} loadingLabel="Duplicating…"
                  onClick={() => duplicateWidget.mutate(selectedWidget.id)}>
                  Duplicate
                </Button>
                <Button variant="dangerSecondary" size="sm" leadingIcon={<Trash2 size={13} />} style={{ flex: 1 }}
                  onClick={() => setDeleteConfirmId(selectedWidget.id)}>
                  Delete
                </Button>
              </div>
            }
          >

            {/* Type badge */}
            <div style={{ marginBottom: 12 }}>
              <div className="mvx-prop-section">Type</div>
              <StatusBadge tone="brand">{selectedWidget.widget_type.replace(/_/g, " ")}</StatusBadge>
            </div>

            {/* Header toggle */}
            <div style={{ marginBottom: 12 }}>
              <div className="mvx-prop-section">Header</div>
              <div style={{ marginBottom: 6 }}>
                <Checkbox
                  checked={propsDraft[selectedWidget.id]?.show_title !== undefined ? propsDraft[selectedWidget.id].show_title : selectedWidget.show_title}
                  onChange={e => setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], show_title: e.target.checked } }))}
                  label="Show title bar"
                />
              </div>
              {(propsDraft[selectedWidget.id]?.show_title !== undefined ? propsDraft[selectedWidget.id].show_title : selectedWidget.show_title) && (
                <TextInput
                  key={`title-${selectedWidget.id}`}
                  placeholder="Widget title…"
                  defaultValue={propsDraft[selectedWidget.id]?.title ?? selectedWidget.title ?? ""}
                  onBlur={e => setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], title: e.target.value || null } }))}
                  style={{ width: "100%" }}
                />
              )}
            </div>

            {/* Surface — every widget type */}
            {(() => {
              const wp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              const isKpi = selectedWidget.widget_type === "metric_kpi";
              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Appearance</div>
                  <Field label="Background">
                    <Select
                      aria-label="Widget background"
                      value={wp.background ?? (isKpi ? "white" : "none")}
                      onChange={e => {
                        const wid = selectedWidget!.id;
                        const v = e.target.value as WidgetBackground;
                        const isDefault = v === (isKpi ? "white" : "none");
                        setPropsDraft(prev => ({ ...prev, [wid]: { ...prev[wid], widget_props: { ...wp, background: isDefault ? undefined : v } } }));
                      }}
                    >
                      <option value="white">White card</option>
                      <option value="none">No background</option>
                    </Select>
                  </Field>
                </div>
              );
            })()}

            {/* Selector placement — grid/chart/KPI */}
            {(selectedWidget.widget_type === "chart" || selectedWidget.widget_type === "grid" || selectedWidget.widget_type === "metric_kpi") && (() => {
              const wp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Selectors</div>
                  <Field label="Position in the widget">
                    <Select
                      aria-label="Selectors position"
                      value={wp.selectors_position ?? "top"}
                      onChange={e => {
                        const wid = selectedWidget!.id;
                        const v = e.target.value as SelectorsPosition;
                        setPropsDraft(prev => ({ ...prev, [wid]: { ...prev[wid], widget_props: { ...wp, selectors_position: v === "top" ? undefined : v } } }));
                      }}
                    >
                      <option value="top">Top edge</option>
                      <option value="bottom">Bottom edge</option>
                      <option value="left">Left column</option>
                      <option value="right">Right column</option>
                    </Select>
                  </Field>
                </div>
              );
            })()}

            {/* Context sync toggle — chart/grid only */}
            {(selectedWidget.widget_type === "chart" || selectedWidget.widget_type === "grid") && (() => {
              const wp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Context sync</div>
                  <Checkbox
                    // Default ON, matching the runtime (a widget syncs unless
                    // it has sync_context===false). Showing `?? false` here
                    // made the box read "off" while widgets actually synced —
                    // misleading (reported live).
                    checked={wp.sync_context !== false}
                    onChange={e => {
                      const wid = selectedWidget!.id;
                      setPropsDraft(prev => ({
                        ...prev,
                        [wid]: { ...prev[wid], widget_props: { ...wp, sync_context: e.target.checked } },
                      }));
                    }}
                    label="Sync context selections with other widgets on this dashboard"
                  />
                  <p style={{ fontSize: 11, color: "var(--color-text-quiet)", marginTop: 4 }}>
                    On by default. When on, widgets sharing a dimension show ONE
                    set of selectors — a user's choice (e.g. period, product)
                    applies to every synced widget at once. Uncheck to give this
                    widget its own independent selectors.
                  </p>
                </div>
              );
            })()}

            {/* Button label + color */}
            {(selectedWidget.widget_type === "automation_button" || selectedWidget.widget_type === "integration_button") && (() => {
              // Literal hex — see the matching comment above; feeds a
              // native color-input value attribute, not a CSS property.
              const defaultColor = selectedWidget.widget_type === "automation_button" ? "#22c55e" : "#3b82f6";
              const currentColor = propsDraft[selectedWidget.id]?.widget_props?.button_color ?? selectedWidget.widget_props?.button_color ?? defaultColor;
              const setButtonColor = (c: string) => setPropsDraft(prev => ({
                ...prev,
                [selectedWidget.id]: {
                  ...prev[selectedWidget.id],
                  widget_props: { ...(selectedWidget.widget_props ?? {}), ...(prev[selectedWidget.id]?.widget_props ?? {}), button_color: c },
                },
              }));
              return (
                <>
                  <div style={{ marginBottom: 12 }}>
                    <div className="mvx-prop-section">Button label</div>
                    <TextInput
                      key={`label-${selectedWidget.id}`}
                      placeholder="Button label…"
                      defaultValue={propsDraft[selectedWidget.id]?.content ?? selectedWidget.content ?? ""}
                      onBlur={e => setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], content: e.target.value } }))}
                      style={{ width: "100%" }}
                    />
                  </div>
                  <div style={{ marginBottom: 12 }}>
                    <div className="mvx-prop-section">Button color</div>
                    <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
                      <input type="color" value={currentColor} onChange={e => setButtonColor(e.target.value)}
                        aria-label="Button color"
                        style={{ width: 32, height: 28, border: "none", borderRadius: 4, cursor: "pointer", padding: 0 }} />
                      <TextInput value={currentColor} onChange={e => setButtonColor(e.target.value)}
                        style={{ flex: 1 }} aria-label="Button color hex" />
                    </div>
                  </div>
                  {selectedWidget.widget_type === "automation_button" && (
                    <div style={{ marginBottom: 12 }}>
                      <div className="mvx-prop-section">Confirmation message</div>
                      <TextInput
                        key={`confirm-${selectedWidget.id}`}
                        placeholder="Ask before running (leave blank to run immediately)…"
                        defaultValue={propsDraft[selectedWidget.id]?.widget_props?.confirm_text ?? selectedWidget.widget_props?.confirm_text ?? ""}
                        onBlur={e => setPropsDraft(prev => ({
                          ...prev,
                          [selectedWidget.id]: {
                            ...prev[selectedWidget.id],
                            widget_props: {
                              ...(selectedWidget.widget_props ?? {}),
                              ...(prev[selectedWidget.id]?.widget_props ?? {}),
                              confirm_text: e.target.value,
                            },
                          },
                        }))}
                        style={{ width: "100%" }}
                      />
                    </div>
                  )}
                </>
              );
            })()}

            {/* Source object selector — not shown for grid, chart, or import widgets (grid is fixed at placement time) */}
            {selectedWidget.widget_type !== "text" && selectedWidget.widget_type !== "grid" && selectedWidget.widget_type !== "chart" && selectedWidget.widget_type !== "import" && (
              <div style={{ marginBottom: 12 }}>
                <div className="mvx-prop-section">
                  {selectedWidget.widget_type === "form" ? "Form source"
                  : selectedWidget.widget_type === "metric_kpi" ? "Metric"
                  : selectedWidget.widget_type === "automation_button" ? "Trigger"
                  : "Integration"}
                </div>
                <Select
                  value={propsDraft[selectedWidget.id]?.ref_id !== undefined ? (propsDraft[selectedWidget.id].ref_id ?? "") : (selectedWidget.ref_id ?? "")}
                  onChange={e => {
                    const val = e.target.value || undefined;
                    setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], ref_id: val } }));
                  }}
                  style={{ width: "100%" }}
                  aria-label="Source object"
                >
                  <option value="">— none —</option>
                  {selectedWidget.widget_type === "form"               && (forms as FormDef[]).map(f => <option key={f.id} value={f.id}>{f.label || f.name}</option>)}
                  {selectedWidget.widget_type === "metric_kpi"         && (metrics as Metric[]).map(m => <option key={m.id} value={m.id}>{m.label || m.name}</option>)}
                  {selectedWidget.widget_type === "automation_button"  && eligibleRules.map(r => <option key={r.id} value={r.id}>{r.name}</option>)}
                  {selectedWidget.widget_type === "integration_button" && (integrations as IntegrationDef[]).map(i => <option key={i.id} value={i.id}>{i.name}</option>)}
                </Select>
              </div>
            )}

            {/* KPI scope — restrict a metric_kpi widget to one dimension
                member (e.g. Region = APAC) instead of the whole-model
                total. Only offered for dimensions the metric is actually
                on (metrics.dimension_ids, from its own grid assignment). */}
            {selectedWidget.widget_type === "metric_kpi" && (() => {
              const metricId = propsDraft[selectedWidget.id]?.ref_id !== undefined ? propsDraft[selectedWidget.id].ref_id : selectedWidget.ref_id;
              const metric = (metrics as Metric[]).find(m => m.id === metricId);
              if (!metric) return null;
              // Scoping now works for calc metrics too (the server resolves a
              // scoped total via a precomputed slice), so every dimensioned
              // metric — input or calculated — can be pinned.
              const scopableDims = (kpiScopeDims as DevDimension[]).filter(d => metric?.dimension_ids?.includes(d.id));

              const currentWp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              const scope = currentWp.kpi_scope;
              const scopeDim = scope ? scopableDims.find(d => d.id === scope.dimension_id) : undefined;
              // Effective mode, defaulting from the legacy fields so existing
              // widgets read the same as before (see MetricKpiWidget).
              const kmode: NonNullable<WidgetProps["kpi_context_mode"]> =
                currentWp.kpi_context_mode ?? (scope ? "pin" : currentWp.sync_context === false ? "total" : "sync");

              function patchWp(patch: Partial<WidgetProps>) {
                const wid = selectedWidget!.id;
                setPropsDraft(prev => ({
                  ...prev,
                  [wid]: { ...prev[wid], widget_props: { ...currentWp, ...patch } },
                }));
              }
              function setScope(next: WidgetProps["kpi_scope"]) { patchWp({ kpi_scope: next }); }

              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Default context</div>
                  <Select
                    value={kmode}
                    onChange={e => {
                      const next = e.target.value as NonNullable<WidgetProps["kpi_context_mode"]>;
                      // When switching INTO pin, seed a scope if none yet; when
                      // leaving pin, drop it so the tile isn't silently pinned.
                      if (next === "pin" && !scope && scopableDims.length > 0) {
                        const d0 = scopableDims[0];
                        const code = defaultLeafCode(d0);
                        patchWp({ kpi_context_mode: next, kpi_scope: code ? { dimension_id: d0.id, member_code: code } : undefined });
                      } else if (next !== "pin") {
                        patchWp({ kpi_context_mode: next, kpi_scope: undefined });
                      } else {
                        patchWp({ kpi_context_mode: next });
                      }
                    }}
                    style={{ width: "100%", marginBottom: kmode === "pin" ? 6 : 0 }}
                    aria-label="KPI default context"
                  >
                    <option value="total">Whole-model total</option>
                    <option value="sync">Follow dashboard selectors</option>
                    {scopableDims.length > 0 && <option value="pin">Pin to a member</option>}
                  </Select>
                  {kmode === "pin" && scopableDims.length > 0 && (
                    <>
                      <Select
                        value={scope?.dimension_id ?? scopableDims[0].id}
                        onChange={e => {
                          const dim = scopableDims.find(d => d.id === e.target.value);
                          const firstCode = dim ? defaultLeafCode(dim) : undefined;
                          setScope(firstCode ? { dimension_id: dim!.id, member_code: firstCode } : undefined);
                        }}
                        style={{ width: "100%", marginBottom: 6 }}
                        aria-label="Scope dimension"
                      >
                        {scopableDims.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
                      </Select>
                      {scope && scopeDim && (
                        <Select
                          value={scope.member_code}
                          onChange={e => setScope({ dimension_id: scope.dimension_id, member_code: e.target.value })}
                          style={{ width: "100%" }}
                          aria-label="Scope member"
                        >
                          {scopeDim.members.map(m => <option key={m.code} value={m.code}>{m.label}</option>)}
                        </Select>
                      )}
                    </>
                  )}
                </div>
              );
            })()}

            {/* Chart configuration editor */}
            {selectedWidget.widget_type === "chart" && demoCtx && (() => {
              const gridRefId = selectedWidget.ref_id ?? "";
              const currentWp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              const currentChart: Partial<GridChartConfig> = currentWp.chart ?? { chart_type: "bar", context_defaults: {} };

              function updateChartProp(patch: Partial<GridChartConfig>) {
                const next = { ...currentChart, ...patch };
                const wid = selectedWidget!.id;
                setPropsDraft(prev => ({
                  ...prev,
                  [wid]: {
                    ...prev[wid],
                    widget_props: { ...currentWp, chart: next as GridChartConfig },
                  },
                }));
              }

              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Chart configuration</div>
                  <ChartWidgetEditor
                    gridDefId={gridRefId}
                    draft={currentChart}
                    ctx={demoCtx as DemoContext}
                    onChange={updateChartProp}
                  />
                </div>
              );
            })()}

            {/* Grid default view editor */}
            {selectedWidget.widget_type === "grid" && (() => {
              if (!selectedGridData) return <p style={{ fontSize: 12, color: "var(--color-disabled)" }}>Loading grid…</p>;

              const METRICS_ID = "__metrics__";
              // Use the resolved dimensions from getGrid — these already handle the
              // revision-redirect so they match exactly what business users will see.
              const dimItems = selectedGridData.dimensions ?? [];
              const allItems: { id: string; label: string; isMetrics: boolean }[] = [
                { id: METRICS_ID, label: "Metrics", isMetrics: true },
                ...dimItems.map(d => ({ id: d.id, label: d.name, isMetrics: false })),
              ];

              const wp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              const dv: GridDefaultView = wp.default_view ?? {
                rows: [METRICS_ID],
                cols: dimItems.length > 0 ? [dimItems[0].id] : [],
                context: dimItems.slice(1).map(d => d.id),
              };

              function setDv(next: GridDefaultView) {
                const wid = selectedWidget!.id;
                setPropsDraft(prev => ({
                  ...prev,
                  [wid]: {
                    ...prev[wid],
                    widget_props: { ...wp, default_view: next },
                  },
                }));
              }

              function zoneOf(id: string): "rows" | "cols" | "context" | "none" {
                if (dv.rows.includes(id)) return "rows";
                if (dv.cols.includes(id)) return "cols";
                if (dv.context.includes(id)) return "context";
                return "none";
              }

              function assignZone(id: string, zone: "rows" | "cols" | "context" | "none") {
                const remove = (arr: string[]) => arr.filter(x => x !== id);
                const next: GridDefaultView = {
                  rows: remove(dv.rows),
                  cols: remove(dv.cols),
                  context: remove(dv.context),
                  filter_sel: dv.filter_sel,
                };
                if (zone !== "none") next[zone] = [...next[zone], id];
                setDv(next);
              }

              const zoneLabel: Record<string, string> = { rows: "Row", cols: "Column", context: "Context", none: "—" };

              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Default view</div>
                  <div style={{ display: "flex", flexDirection: "column", gap: 4 }}>
                    {allItems.map(item => {
                      const zone = zoneOf(item.id);
                      return (
                        <div key={item.id} style={{ display: "flex", alignItems: "center", gap: 6, padding: "4px 8px", borderRadius: "var(--radius-button)", background: "var(--color-surface-subtle)", border: "1px solid var(--color-border)" }}>
                          <span className="mvx-admin-mono" style={{ fontWeight: 700, color: item.isMetrics ? "var(--color-brand-600)" : "var(--color-text-subtle)", width: 14, flexShrink: 0 }}>
                            {item.isMetrics ? "fx" : "≡"}
                          </span>
                          <span style={{ flex: 1, fontSize: 12, fontWeight: 500, overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>
                            {item.label}
                          </span>
                          <Select
                            value={zone}
                            onChange={e => assignZone(item.id, e.target.value as "rows" | "cols" | "context" | "none")}
                            aria-label={`Zone for ${item.label}`}
                          >
                            {(["rows", "cols", "context", "none"] as const).map(z => (
                              <option key={z} value={z}>{zoneLabel[z]}</option>
                            ))}
                          </Select>
                        </div>
                      );
                    })}
                  </div>
                </div>
              );
            })()}

            {/* Text content editor */}
            {selectedWidget.widget_type === "text" && (
              <div style={{ marginBottom: 12 }}>
                <div className="mvx-prop-section">Content</div>
                <div className="mvx-admin-muted" style={{ fontSize: 12, marginBottom: 4 }}>
                  Markdown: <code># heading</code>, <code>**bold**</code>, <code>- bullets</code>, <code>[label](https://example.com)</code>
                </div>
                <Textarea
                  key={selectedWidget.id}
                  defaultValue={propsDraft[selectedWidget.id]?.content ?? selectedWidget.content ?? ""}
                  onBlur={e => {
                    setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], content: e.target.value } }));
                  }}
                  rows={4}
                  style={{ width: "100%", resize: "vertical" }}
                  aria-label="Text content"
                />
              </div>
            )}

            {/* Image: replace the picture, its alt text and how it fills the box */}
            {selectedWidget.widget_type === "image" && (() => {
              const wp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              const setWp = (patch: WidgetProps) =>
                setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], widget_props: { ...wp, ...patch } } }));
              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Image</div>
                  <input
                    type="file"
                    accept="image/*"
                    aria-label="Replace the image"
                    onChange={async e => {
                      const file = e.target.files?.[0];
                      if (!file) return;
                      try {
                        setImageError("");
                        const data = await readImageAsDataURL(file);
                        setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], content: data } }));
                      } catch (err) { setImageError((err as Error).message); }
                    }}
                  />
                  {imageError && <div className="mvx-admin-error">{imageError}</div>}
                  <Field label="Alternative text" description="Read aloud by screen readers, and shown if the picture cannot load.">
                    <TextInput
                      defaultValue={wp.alt ?? ""}
                      onBlur={e => setWp({ alt: e.target.value })}
                      placeholder="What the picture shows"
                    />
                  </Field>
                  <Field label="Fit">
                    <Select value={wp.image_fit ?? "contain"} onChange={e => setWp({ image_fit: e.target.value as "contain" | "cover" })}>
                      <option value="contain">Show all of it</option>
                      <option value="cover">Fill the box</option>
                    </Select>
                  </Field>
                </div>
              );
            })()}

            {/* Text style */}
            {selectedWidget.widget_type === "text" && (() => {
              const wp = { ...(selectedWidget.widget_props ?? {}), ...(propsDraft[selectedWidget.id]?.widget_props ?? {}) };
              const setWp = (patch: WidgetProps) =>
                setPropsDraft(prev => ({ ...prev, [selectedWidget.id]: { ...prev[selectedWidget.id], widget_props: { ...wp, ...patch } } }));
              return (
                <div style={{ marginBottom: 12 }}>
                  <div className="mvx-prop-section">Text style</div>
                  <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 6, marginBottom: 6 }}>
                    <Field label="Size (px)">
                      <NumberInput
                        min={8} max={120}
                        value={wp.font_size ?? 14}
                        onChange={e => setWp({ font_size: Number(e.target.value) })}
                        style={{ width: "100%" }}
                      />
                    </Field>
                    <Field label="Weight">
                      <Select
                        value={wp.font_weight ?? "normal"}
                        onChange={e => setWp({ font_weight: e.target.value })}
                        style={{ width: "100%" }}
                      >
                        <option value="normal">Normal</option>
                        <option value="600">Semi-bold</option>
                        <option value="bold">Bold</option>
                      </Select>
                    </Field>
                  </div>
                  <Field label="Font family" style={{ marginBottom: 6 }}>
                    <Select
                      value={wp.font_family ?? "sans-serif"}
                      onChange={e => setWp({ font_family: e.target.value })}
                      style={{ width: "100%" }}
                    >
                      <option value="sans-serif">Sans-serif</option>
                      <option value="serif">Serif</option>
                      <option value="monospace">Monospace</option>
                    </Select>
                  </Field>
                  <Field label="Color">
                    {/* The two defaults below are literal hex, not design
                        tokens: the native color-input value attribute and
                        its paired hex-text display must show a real hex
                        string, not the literal text "var(--color-text)". */}
                    <div style={{ display: "flex", gap: 6, alignItems: "center" }}>
                      <input
                        type="color"
                        value={wp.color ?? "#111827"}
                        onChange={e => setWp({ color: e.target.value })}
                        aria-label="Text color"
                        style={{ width: 32, height: 28, border: "none", borderRadius: 4, cursor: "pointer", padding: 0 }}
                      />
                      <TextInput
                        value={wp.color ?? "#111827"}
                        onChange={e => setWp({ color: e.target.value })}
                        style={{ flex: 1 }}
                        aria-label="Text color hex"
                      />
                    </div>
                  </Field>
                </div>
              );
            })()}

            {/* Layout info */}
            {(() => {
              const sr = getRect(selectedWidget);
              const { minW, minH } = getWidgetSizes(selectedWidget.widget_type);
              const setSize = (w: number, h: number) => {
                draftRef.current[selectedWidget.id] = { ...sr, w: Math.max(minW, snap(w)), h: Math.max(minH, snap(h)) };
                forceRender();
              };
              return (<>
                <div style={{ marginBottom: 8 }}>
                  <div className="mvx-prop-section">Position</div>
                  <div style={{ fontSize: 12 }}>x: {sr.x} · y: {sr.y}</div>
                </div>
                <div style={{ marginBottom: 14 }}>
                  <div className="mvx-prop-section">Size</div>
                  <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 6 }}>
                    <Field label="Width (px)">
                      <NumberInput min={minW} step={SNAP} value={sr.w}
                        onChange={e => setSize(Number(e.target.value), sr.h)}
                        style={{ width: "100%" }} />
                    </Field>
                    <Field label="Height (px)">
                      <NumberInput min={minH} step={SNAP} value={sr.h}
                        onChange={e => setSize(sr.w, Number(e.target.value))}
                        style={{ width: "100%" }} />
                    </Field>
                  </div>
                </div>
              </>);
            })()}
          </PropertyPanel>
        )}
      </div>

      {/* Footer hint */}
      {mode === "design" && (
        <div style={{ marginTop: 6, fontSize: 11, color: "var(--color-disabled)", textAlign: "center" }}>
          Click to select · Drag header to move · Drag edges/corner to resize · Arrow keys to nudge · Shift+Arrow for larger step
        </div>
      )}

      {/* Delete confirmation modal */}
      {(() => {
        const w = deleteConfirmId ? widgets.find(ww => ww.id === deleteConfirmId) : undefined;
        return (
          <ConfirmDialog
            open={!!w}
            title="Delete widget?"
            body={w ? (
              <>
                Remove <strong>"{widgetSourceLabel(w)}"</strong> ({w.widget_type.replace(/_/g, " ")}) from this dashboard?
                The underlying source object will not be deleted.
              </>
            ) : ""}
            confirmLabel={removeWidget.isPending ? "Deleting…" : "Delete"}
            onConfirm={() => { if (deleteConfirmId) removeWidget.mutate(deleteConfirmId); }}
            onCancel={() => setDeleteConfirmId(null)}
          />
        );
      })()}
    </div>
  );
}
