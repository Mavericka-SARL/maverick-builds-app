import { useEffect, useMemo, useRef, useState } from "react";
import { Crosshair, ZoomIn, ZoomOut, Maximize } from "lucide-react";
import type { DevModel, DevMetric, DevDimension, GridDef } from "../../api/client";
import { IconButton, SearchInput } from "../../ui";

const NODE_W = 180;
const NODE_H = 72;
const COL_GAP = 220;
const ROW_GAP = 72;
const PAD = 24;

const ZOOM_MIN = 0.25;
const ZOOM_MAX = 2.5;
const ZOOM_STEP = 1.2;

function truncateLabel(s: string, max: number): string {
  return s.length > max ? s.slice(0, max - 1) + "…" : s;
}

export function DepGraph({ model, grids, dims }: { model: DevModel; grids: GridDef[]; dims: DevDimension[] }) {
  const [zoom, setZoom] = useState(1);
  const [selected, setSelected] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  const scrollRef = useRef<HTMLDivElement>(null);

  const dimNameById = new Map(dims.map((d) => [d.id, d.name]));
  const gridsByMetricId = new Map<string, GridDef[]>();
  grids.forEach((g) => {
    g.metric_ids.forEach((mid) => {
      const arr = gridsByMetricId.get(mid) ?? [];
      arr.push(g);
      gridsByMetricId.set(mid, arr);
    });
  });
  // A metric's "connected dimensions" are the dimensions of every grid that
  // includes it — a metric has no dimensions of its own, only through the
  // grids it's placed on.
  function connectionsFor(metricId: string): { gridNames: string[]; dimNames: string[] } {
    const connectedGrids = gridsByMetricId.get(metricId) ?? [];
    const dimNames = [...new Set(connectedGrids.flatMap((g) => g.dimension_ids).map((id) => dimNameById.get(id) ?? id))];
    return { gridNames: connectedGrids.map((g) => g.name), dimNames };
  }

  const depthMap: Record<string, number> = {};

  function depth(name: string): number {
    if (depthMap[name] !== undefined) return depthMap[name];
    const m = model.metrics.find((x) => x.name === name);
    if (!m || m.is_input) return (depthMap[name] = 0);
    const d = 1 + Math.max(0, ...m.depends_on.map(depth));
    depthMap[name] = d;
    return d;
  }

  model.metrics.forEach((m) => depth(m.name));

  const cols: Record<number, DevMetric[]> = {};
  model.metrics.forEach((m) => {
    const d = depthMap[m.name] ?? 0;
    (cols[d] ??= []).push(m);
  });

  const positions: Record<string, { x: number; y: number }> = {};
  Object.entries(cols).forEach(([col, metrics]) => {
    const c = parseInt(col);
    metrics.forEach((m, i) => {
      positions[m.name] = {
        x: PAD + c * (NODE_W + COL_GAP),
        y: PAD + i * (NODE_H + ROW_GAP),
      };
    });
  });

  const maxCol = Math.max(...Object.keys(cols).map(Number));
  const maxRow = Math.max(...Object.values(cols).map((c) => c.length));
  const svgW = PAD * 2 + (maxCol + 1) * (NODE_W + COL_GAP) - COL_GAP + NODE_W;
  const svgH = PAD * 2 + maxRow * (NODE_H + ROW_GAP);

  const edges: { from: string; to: string; x1: number; y1: number; x2: number; y2: number }[] = [];
  model.metrics.forEach((m) => {
    const to = positions[m.name];
    if (!to) return;
    m.depends_on.forEach((dep) => {
      const from = positions[dep];
      if (!from) return;
      edges.push({
        from: dep, to: m.name,
        x1: from.x + NODE_W, y1: from.y + NODE_H / 2,
        x2: to.x, y2: to.y + NODE_H / 2,
      });
    });
  });

  // The selection's one-hop neighborhood: the metric itself, everything it
  // reads (upstream), and everything that reads it (downstream). Everything
  // else fades so the clicked metric's immediate data flow stands out.
  const neighborhood = useMemo(() => {
    if (!selected) return null;
    const m = model.metrics.find((x) => x.name === selected);
    const set = new Set<string>([selected]);
    (m?.depends_on ?? []).forEach((d) => set.add(d));
    model.metrics.forEach((other) => {
      if (other.depends_on.includes(selected)) set.add(other.name);
    });
    return set;
  }, [selected, model.metrics]);

  const clampZoom = (z: number) => Math.min(ZOOM_MAX, Math.max(ZOOM_MIN, z));
  const zoomBy = (factor: number) => setZoom((z) => clampZoom(z * factor));

  // Scroll the viewport so the named node sits centered, at the current
  // zoom (or an explicit one, for select-and-zoom in one step).
  const centerOn = (name: string, atZoom?: number) => {
    const pos = positions[name];
    const el = scrollRef.current;
    if (!pos || !el) return;
    const z = atZoom ?? zoom;
    el.scrollTo({
      left: (pos.x + NODE_W / 2) * z - el.clientWidth / 2,
      top: (pos.y + NODE_H / 2) * z - el.clientHeight / 2,
      behavior: "smooth",
    });
  };

  const searchMatches = search.trim()
    ? model.metrics.filter(m =>
        m.label.toLowerCase().includes(search.trim().toLowerCase()) ||
        m.name.toLowerCase().includes(search.trim().toLowerCase()))
    : [];
  const jumpTo = (name: string) => {
    setSelected(name);
    // Reading scroll geometry is fine synchronously; the zoom bump (if
    // any) applies via state and the scroll uses the target zoom value.
    const targetZoom = Math.max(zoom, 1);
    if (targetZoom !== zoom) setZoom(targetZoom);
    // Defer one frame so a zoom change has resized the svg before scrolling.
    requestAnimationFrame(() => centerOn(name, targetZoom));
  };

  // Ctrl+wheel (and trackpad pinch, which browsers report the same way)
  // zooms; a plain wheel keeps scrolling. Attached natively because React
  // registers onWheel as passive, so preventDefault there cannot stop the
  // browser's own page zoom.
  useEffect(() => {
    const el = scrollRef.current;
    if (!el) return;
    const onWheel = (e: WheelEvent) => {
      if (!e.ctrlKey && !e.metaKey) return;
      e.preventDefault();
      zoomBy(e.deltaY < 0 ? ZOOM_STEP : 1 / ZOOM_STEP);
    };
    el.addEventListener("wheel", onWheel, { passive: false });
    return () => el.removeEventListener("wheel", onWheel);
    // eslint-disable-next-line react-hooks/exhaustive-deps -- zoomBy only wraps a setState updater
  }, []);

  return (
    <div style={{ position: "relative" }}>
      <div style={{ position: "absolute", top: 8, right: 8, zIndex: 5, display: "flex", gap: 4, alignItems: "center", background: "var(--color-surface)", border: "1px solid var(--color-border)", borderRadius: "var(--radius-button)", padding: 3 }}>
        <div style={{ position: "relative" }}>
          <SearchInput
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            placeholder="Find metric…"
            width={170}
            onKeyDown={(e) => {
              if (e.key === "Enter" && searchMatches.length > 0) {
                jumpTo(searchMatches[0].name);
                setSearch("");
              }
              if (e.key === "Escape") setSearch("");
            }}
          />
          {searchMatches.length > 0 && (
            <div style={{ position: "absolute", top: "100%", left: 0, marginTop: 4, minWidth: 220, maxHeight: 240, overflowY: "auto", background: "var(--color-surface)", border: "1px solid var(--color-border)", borderRadius: "var(--radius-button)", boxShadow: "var(--shadow-popover)", zIndex: 6 }}>
              {searchMatches.slice(0, 12).map(m => (
                <button
                  key={m.name}
                  type="button"
                  style={{ display: "block", width: "100%", textAlign: "left", padding: "6px 10px", border: "none", background: "transparent", cursor: "pointer", fontSize: 12, fontFamily: "inherit" }}
                  onClick={() => { jumpTo(m.name); setSearch(""); }}
                >
                  {m.label} <span className="mvx-admin-muted">({m.is_input ? "input" : "calc"})</span>
                </button>
              ))}
            </div>
          )}
        </div>
        <IconButton
          aria-label="Zoom to selection"
          title={selected ? "Center the selected metric" : "Select a metric first"}
          disabled={!selected}
          onClick={() => selected && jumpTo(selected)}
        >
          <Crosshair size={15} />
        </IconButton>
        <IconButton aria-label="Zoom out" title="Zoom out" onClick={() => zoomBy(1 / ZOOM_STEP)}><ZoomOut size={15} /></IconButton>
        <span style={{ alignSelf: "center", fontSize: 11, minWidth: 38, textAlign: "center", color: "var(--color-text-muted)", fontVariantNumeric: "tabular-nums" }}>
          {Math.round(zoom * 100)}%
        </span>
        <IconButton aria-label="Zoom in" title="Zoom in" onClick={() => zoomBy(ZOOM_STEP)}><ZoomIn size={15} /></IconButton>
        <IconButton aria-label="Reset zoom" title="Reset zoom" onClick={() => setZoom(1)}><Maximize size={15} /></IconButton>
      </div>
      <div ref={scrollRef} style={{ overflow: "auto", maxHeight: "calc(100vh - 220px)" }}>
        <svg
          width={svgW * zoom}
          height={svgH * zoom}
          viewBox={`0 0 ${svgW} ${svgH}`}
          style={{ display: "block" }}
          onClick={() => setSelected(null)}
        >
          <defs>
            <marker id="arrow" markerWidth="8" markerHeight="8" refX="6" refY="3" orient="auto">
              <path d="M0,0 L0,6 L8,3 z" fill="var(--color-text-subtle)" />
            </marker>
            <marker id="arrow-active" markerWidth="8" markerHeight="8" refX="6" refY="3" orient="auto">
              <path d="M0,0 L0,6 L8,3 z" fill="var(--color-brand-600)" />
            </marker>
          </defs>
          {edges.map((e, i) => {
            const active = selected !== null && (e.from === selected || e.to === selected);
            const dimmed = selected !== null && !active;
            return (
              <path key={i}
                d={`M${e.x1},${e.y1} C${e.x1 + 60},${e.y1} ${e.x2 - 60},${e.y2} ${e.x2},${e.y2}`}
                fill="none"
                stroke={active ? "var(--color-brand-600)" : "var(--color-text-subtle)"}
                strokeWidth={active ? 2.5 : 1.5}
                opacity={dimmed ? 0.15 : 1}
                markerEnd={active ? "url(#arrow-active)" : "url(#arrow)"}
              />
            );
          })}
          {model.metrics.map((m) => {
            const pos = positions[m.name];
            if (!pos) return null;
            const { gridNames, dimNames } = connectionsFor(m.id);
            const gridsLabel = gridNames.length ? truncateLabel(gridNames.join(", "), 26) : "No grid";
            const dimsLabel = dimNames.length ? truncateLabel(dimNames.join(", "), 26) : "—";
            const isSelected = selected === m.name;
            const inHood = neighborhood === null || neighborhood.has(m.name);
            return (
              <g
                key={m.name}
                transform={`translate(${pos.x},${pos.y})`}
                opacity={inHood ? 1 : 0.25}
                style={{ cursor: "pointer" }}
                onClick={(e) => {
                  e.stopPropagation();
                  setSelected((cur) => (cur === m.name ? null : m.name));
                }}
              >
                <title>
                  {`${m.label}${gridNames.length ? `\nGrids: ${gridNames.join(", ")}` : "\nNot on any grid"}${dimNames.length ? `\nDimensions: ${dimNames.join(", ")}` : ""}`}
                </title>
                <rect width={NODE_W} height={NODE_H} rx={6}
                  fill={m.is_input ? "var(--color-diagram-input-bg)" : "var(--color-diagram-calc-bg)"}
                  stroke={isSelected ? "var(--color-brand-600)" : m.is_input ? "var(--color-diagram-input-border)" : "var(--color-diagram-calc-border)"}
                  strokeWidth={isSelected ? 2.5 : 1.5}
                />
                <text x={NODE_W / 2} y={14} textAnchor="middle" fontSize={10} fill="var(--color-text-quiet)" fontFamily="ui-monospace, monospace">
                  {m.is_input ? "INPUT" : "CALC"}
                </text>
                <text x={NODE_W / 2} y={30} textAnchor="middle" fontSize={12} fill="var(--color-text)" fontWeight={600} fontFamily="system-ui, sans-serif">
                  {m.label}
                </text>
                <text x={NODE_W / 2} y={47} textAnchor="middle" fontSize={9.5} fontFamily="system-ui, sans-serif"
                  fill={gridNames.length ? "var(--color-text-quiet)" : "var(--color-warning)"}>
                  {gridNames.length ? `Grid: ${gridsLabel}` : "Not on any grid"}
                </text>
                <text x={NODE_W / 2} y={61} textAnchor="middle" fontSize={9.5} fill="var(--color-text-quiet)" fontFamily="system-ui, sans-serif">
                  {dimNames.length ? `Dim: ${dimsLabel}` : ""}
                </text>
              </g>
            );
          })}
        </svg>
      </div>
    </div>
  );
}
