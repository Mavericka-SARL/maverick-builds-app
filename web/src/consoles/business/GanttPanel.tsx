/**
 * A table's records as a Gantt chart (widget_type "gantt"): each record a
 * bar from its start to its end date, its baseline drawn under it, the part
 * done filled, a record without an end a milestone, and an arrow from the
 * record it follows — red when it starts before that one ends. Until
 * 2026-11-27 a timeline was a grid of glyphs coloured by highlight rules and
 * the dependencies were checked, not drawn (development-programme build).
 *
 * The records are read as the records widget reads them (POST
 * /api/forms/{id}/records/query): the reader's access, the widget's fixed
 * filters and the dashboard selection it follows.
 */
import { useMemo, useState } from "react";
import { useQuery, keepPreviousData } from "@tanstack/react-query";
import { api, type FormDef, type FormRecord, type GanttSettings, type RecordFilter, type DevDimension } from "../../api/client";
import { LoadingState, EmptyState, ErrorState } from "../../ui";

type Zoom = "month" | "quarter" | "year";
const DAY_PX: Record<Zoom, number> = { month: 6, quarter: 2, year: 0.7 };
const ROW_H = 30;
const LABEL_W = 240;
const PALETTE = ["--color-chart-1", "--color-chart-2", "--color-chart-3", "--color-chart-4", "--color-chart-5",
  "--color-chart-6", "--color-chart-7", "--color-chart-8", "--color-chart-9", "--color-chart-10"];
const DAY = 86_400_000;
// The width a timeline fits in before it scrolls: a full-width widget's.
const FIT_PX = 860;

/** The finest zoom showing days days within FIT_PX. */
function autoZoom(days: number): Zoom {
  if (days * DAY_PX.month <= FIT_PX) return "month";
  if (days * DAY_PX.quarter <= FIT_PX) return "quarter";
  return "year";
}

const dateOf = (v: unknown): number | null => {
  if (typeof v !== "string" || !/^\d{4}-\d{2}-\d{2}/.test(v)) return null;
  const t = Date.parse(v.slice(0, 10) + "T00:00:00Z");
  return Number.isNaN(t) ? null : t;
};
const text = (v: unknown): string => (v == null ? "" : typeof v === "string" ? v : String(v));
const fmt = (t: number) => new Date(t).toISOString().slice(0, 10);

interface Bar {
  rec: FormRecord;
  code: string;
  label: string;
  group: string;
  start: number;
  end: number | null;
  baseStart: number | null;
  baseEnd: number | null;
  progress: number | null;
  color: string;
  after: string;
}

export function GanttPanel({ form, settings, fixedFilters, followFilters, title }: {
  form: FormDef;
  revisionId: string;
  settings: GanttSettings;
  fixedFilters?: RecordFilter[];
  followFilters?: RecordFilter[];
  title?: string;
}) {
  // null: the finest zoom that shows the whole span (autoZoom); a person's choice once made.
  const [chosenZoom, setZoom] = useState<Zoom | null>(null);
  // Today's line: the day the widget opened.
  const [today] = useState(() => Date.now());
  const filters = useMemo(() => [...(fixedFilters ?? []), ...(followFilters ?? [])], [fixedFilters, followFilters]);
  const { data: page, isLoading, error } = useQuery({
    queryKey: ["gantt", form.id, JSON.stringify(filters), settings.start_field],
    queryFn: () => api.queryRecords(form.id, { filters, sort: [{ field: settings.start_field }], limit: 1000 }),
    placeholderData: keepPreviousData,
    refetchInterval: 20_000,
  });
  const { data: dimsRaw = [] } = useQuery({ queryKey: ["dimensions"], queryFn: api.getDimensions });

  const fieldOf = (name?: string) => (name ? form.fields.find(f => f.name === name) : undefined);
  const codeField = form.fields.find(f => f.type === "autocode") ?? form.fields.find(f => f.code);
  const labelField = fieldOf(settings.label_field) ?? (form.member_label_field ? fieldOf(form.member_label_field) : undefined) ?? form.fields.find(f => f.type === "text");
  // A dimension field's value is a member code: shown by the member's label.
  const memberLabel = useMemo(() => {
    const byDim = new Map<string, Map<string, string>>();
    for (const d of dimsRaw as DevDimension[]) byDim.set(d.id, new Map((d.members ?? []).map(m => [m.code, m.label || m.code])));
    return (fieldName: string | undefined, v: unknown) => {
      const f = fieldOf(fieldName);
      const s = text(v);
      return f?.type === "dimension" && f.dimension_id ? byDim.get(f.dimension_id)?.get(s) ?? s : s;
    };
  }, [dimsRaw, form]); // eslint-disable-line react-hooks/exhaustive-deps

  const bars: Bar[] = useMemo(() => {
    const colors = new Map<string, string>();
    const out: Bar[] = [];
    for (const rec of page?.records ?? []) {
      const start = dateOf(rec.data[settings.start_field]);
      if (start == null) continue;
      const colorKey = settings.color_field ? (settings.color_field === "status" ? rec.status : text(rec.data[settings.color_field])) : "";
      if (!colors.has(colorKey)) colors.set(colorKey, PALETTE[colors.size % PALETTE.length]);
      const p = settings.progress_field ? Number(rec.data[settings.progress_field]) : NaN;
      out.push({
        rec,
        code: codeField ? text(rec.data[codeField.name]) : rec.id,
        label: labelField ? text(rec.data[labelField.name]) : rec.id,
        group: settings.group_field ? memberLabel(settings.group_field, rec.data[settings.group_field]) : "",
        start,
        end: settings.end_field ? dateOf(rec.data[settings.end_field]) : null,
        baseStart: settings.baseline_start_field ? dateOf(rec.data[settings.baseline_start_field]) : null,
        baseEnd: settings.baseline_end_field ? dateOf(rec.data[settings.baseline_end_field]) : null,
        progress: Number.isFinite(p) ? Math.max(0, Math.min(1, p > 1 ? p / 100 : p)) : null,
        color: `var(${colors.get(colorKey)})`,
        after: settings.predecessor_field ? text(rec.data[settings.predecessor_field]) : "",
      });
    }
    out.sort((a, b) => a.group.localeCompare(b.group) || a.start - b.start);
    return out;
  }, [page, settings, codeField, labelField, memberLabel]);

  if (isLoading && !page) return <LoadingState label="Loading the timeline…" />;
  if (error) return <ErrorState message={(error as Error).message} />;
  if (bars.length === 0) return <EmptyState label={`No ${form.label || form.name} with a ${fieldOf(settings.start_field)?.label ?? settings.start_field} to show.`} />;

  // The span: every bar and baseline, padded to whole months.
  let lo = Infinity, hi = -Infinity;
  for (const b of bars) {
    for (const t of [b.start, b.end, b.baseStart, b.baseEnd]) if (t != null) { lo = Math.min(lo, t); hi = Math.max(hi, t); }
  }
  const first = new Date(lo); const from = Date.UTC(first.getUTCFullYear(), first.getUTCMonth(), 1);
  const last = new Date(hi); const to = Date.UTC(last.getUTCFullYear(), last.getUTCMonth() + 1, 1);
  const zoom: Zoom = chosenZoom ?? autoZoom((to - from) / DAY);
  const px = DAY_PX[zoom];
  const x = (t: number) => ((t - from) / DAY) * px;
  const width = Math.max(x(to), 200);

  // Rows: a group heading, then its bars.
  type Row = { kind: "group"; label: string } | { kind: "bar"; bar: Bar };
  const rows: Row[] = [];
  let prev: string | null = null;
  for (const b of bars) {
    if (settings.group_field && b.group !== prev) { rows.push({ kind: "group", label: b.group || "—" }); prev = b.group; }
    rows.push({ kind: "bar", bar: b });
  }
  const rowOf = new Map<string, number>();
  rows.forEach((r, i) => { if (r.kind === "bar" && r.bar.code) rowOf.set(r.bar.code, i); });
  const height = rows.length * ROW_H;

  // The scale: months (quarters, years when zoomed out).
  const ticks: { t: number; label: string }[] = [];
  for (let d = new Date(from); d.getTime() < to; d = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + 1, 1))) {
    const m = d.getUTCMonth();
    if (zoom === "month" || (zoom === "quarter" && m % 3 === 0) || (zoom === "year" && m === 0)) {
      const label = zoom === "year" ? String(d.getUTCFullYear())
        : zoom === "quarter" ? `Q${m / 3 + 1} ${d.getUTCFullYear()}`
        : d.toLocaleString(undefined, { month: "short", year: m === 0 ? "numeric" : undefined, timeZone: "UTC" });
      ticks.push({ t: d.getTime(), label });
    }
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%" }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, padding: "8px 12px", borderBottom: "1px solid var(--color-border)" }}>
        {title && <strong style={{ fontSize: 13 }}>{title}</strong>}
        <span className="mvx-admin-muted" style={{ fontSize: 12 }}>{bars.length} {form.label || form.name}</span>
        <span style={{ flex: 1 }} />
        <label className="mvx-admin-muted" style={{ fontSize: 12 }}>
          Zoom{" "}
          <select value={zoom} onChange={e => setZoom(e.target.value as Zoom)} aria-label="Timeline zoom">
            <option value="month">Months</option>
            <option value="quarter">Quarters</option>
            <option value="year">Years</option>
          </select>
        </label>
      </div>
      <div style={{ overflow: "auto", flex: 1 }} data-testid="gantt">
        <div style={{ display: "grid", gridTemplateColumns: `${LABEL_W}px ${width}px`, minWidth: LABEL_W + width }}>
          <div style={{ position: "sticky", left: 0, top: 0, zIndex: 3, background: "var(--color-surface)", height: 28, borderBottom: "1px solid var(--color-border)" }} />
          <svg width={width} height={28} style={{ position: "sticky", top: 0, zIndex: 2, background: "var(--color-surface)", borderBottom: "1px solid var(--color-border)" }}>
            {ticks.map(tk => (
              <g key={tk.t}>
                <line x1={x(tk.t)} x2={x(tk.t)} y1={0} y2={28} stroke="var(--color-border)" />
                <text x={x(tk.t) + 4} y={18} fontSize={11} fill="var(--color-text-muted)">{tk.label}</text>
              </g>
            ))}
          </svg>
          <div style={{ position: "sticky", left: 0, zIndex: 1, background: "var(--color-surface)", borderRight: "1px solid var(--color-border)" }}>
            {rows.map((r, i) => (
              <div key={i} style={{ minHeight: ROW_H, height: ROW_H, display: "flex", alignItems: "center", padding: "0 8px", fontSize: 12,
                fontWeight: r.kind === "group" ? 700 : 400, background: r.kind === "group" ? "var(--color-surface-subtle)" : undefined,
                overflowWrap: "anywhere", lineHeight: 1.2 }}>
                {r.kind === "group" ? r.label : <span style={{ paddingLeft: settings.group_field ? 10 : 0 }}>{r.bar.label}</span>}
              </div>
            ))}
          </div>
          <svg width={width} height={height} role="img" aria-label={`Timeline of ${bars.length} ${form.label || form.name}`}>
            {ticks.map(tk => <line key={tk.t} x1={x(tk.t)} x2={x(tk.t)} y1={0} y2={height} stroke="var(--color-border)" strokeOpacity={0.5} />)}
            {rows.map((r, i) => r.kind === "group"
              ? <rect key={i} x={0} y={i * ROW_H} width={width} height={ROW_H} fill="var(--color-surface-subtle)" />
              : null)}
            {/* Dependencies under the bars. */}
            {rows.map((r, i) => {
              if (r.kind !== "bar" || !r.bar.after) return null;
              const j = rowOf.get(r.bar.after);
              if (j == null) return null;
              const pre = (rows[j] as { kind: "bar"; bar: Bar }).bar;
              const preEnd = pre.end ?? pre.start;
              const late = r.bar.start < preEnd; // starts before what it follows ends
              const x1 = x(preEnd), y1 = j * ROW_H + ROW_H / 2, x2 = x(r.bar.start), y2 = i * ROW_H + ROW_H / 2;
              const mid = Math.max(x1 + 6, x2 - 6);
              return (
                <path key={`dep-${i}`} d={`M${x1},${y1} H${mid} V${y2} H${x2}`} fill="none"
                  stroke={late ? "var(--color-danger)" : "var(--color-text-muted)"} strokeWidth={late ? 1.6 : 1} markerEnd="url(#gantt-arrow)">
                  <title>{`${r.bar.label} follows ${pre.label}${late ? ` — but starts ${fmt(r.bar.start)}, before it ends ${fmt(preEnd)}` : ""}`}</title>
                </path>
              );
            })}
            <defs>
              <marker id="gantt-arrow" viewBox="0 0 6 6" refX={5} refY={3} markerWidth={6} markerHeight={6} orient="auto">
                <path d="M0,0 L6,3 L0,6 z" fill="var(--color-text-muted)" />
              </marker>
            </defs>
            {rows.map((r, i) => {
              if (r.kind !== "bar") return null;
              const b = r.bar;
              const y = i * ROW_H;
              const tip = `${b.label}: ${fmt(b.start)}${b.end != null && b.end !== b.start ? ` – ${fmt(b.end)}` : ""}` +
                (b.baseStart != null ? ` (baseline ${fmt(b.baseStart)}${b.baseEnd != null ? ` – ${fmt(b.baseEnd)}` : ""})` : "") +
                (b.progress != null ? `, ${Math.round(b.progress * 100)}% done` : "");
              const base = b.baseStart != null && (
                <rect x={x(b.baseStart)} y={y + ROW_H - 8} width={Math.max(2, x((b.baseEnd ?? b.baseStart) + DAY) - x(b.baseStart))} height={4}
                  fill="var(--color-text-muted)" opacity={0.45} rx={1} />
              );
              if (b.end == null || b.end <= b.start) {
                const cx = x(b.start), cy = y + ROW_H / 2 - 2;
                return (
                  <g key={i} data-gantt-bar={b.code}>
                    {base}
                    <path d={`M${cx},${cy - 7} L${cx + 7},${cy} L${cx},${cy + 7} L${cx - 7},${cy} z`} fill={b.color}><title>{tip}</title></path>
                  </g>
                );
              }
              const w = Math.max(3, x(b.end + DAY) - x(b.start));
              return (
                <g key={i} data-gantt-bar={b.code}>
                  {base}
                  <rect x={x(b.start)} y={y + 6} width={w} height={ROW_H - 16} rx={3} fill={b.color} opacity={b.progress != null ? 0.45 : 0.9}><title>{tip}</title></rect>
                  {b.progress != null && <rect x={x(b.start)} y={y + 6} width={w * b.progress} height={ROW_H - 16} rx={3} fill={b.color}><title>{tip}</title></rect>}
                </g>
              );
            })}
            {today >= from && today <= to && (
              <line x1={x(today)} x2={x(today)} y1={0} y2={height} stroke="var(--color-danger)" strokeDasharray="4 3"><title>Today</title></line>
            )}
          </svg>
        </div>
      </div>
    </div>
  );
}
