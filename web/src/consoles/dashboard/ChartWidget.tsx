import React, { useState, useEffect, useContext } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  ResponsiveContainer,
  BarChart, Bar,
  LineChart, Line,
  PieChart, Pie, Cell,
  ScatterChart, Scatter, XAxis, YAxis, CartesianGrid, Tooltip, Legend,
  LabelList,
} from "recharts";
import {
  api,
  type DashboardWidget,
  type DemoContext,
  type GridChartConfig,
  type CategoryChartData,
  type ScatterChartData,
  type HistogramChartData,
  type ChartData,
  type ChartContextDim,
} from "../../api/client";
import { CHART_COLORS, formatValue, CHART_TYPE_LABELS, isChartConfigComplete } from "./chartTypes";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";
import { defaultLeafCode } from "../dashboardLayout";
import { useSelectorOwnership, useSyncSetter, useWidgetContextSync, DashboardContextSyncContext } from "../dashboardContextSync";

// ── Value formatter for axes ──────────────────────────────────────────────────

function makeAxisFormatter(format: string | undefined, currencySymbol: string) {
  return (v: number) => formatValue(v, format, currencySymbol);
}

// ── Context selector row ──────────────────────────────────────────────────────

export function ContextSelectors({
  dims,
  context,
  onChange,
  vertical = false,
}: {
  dims: ChartContextDim[];
  context: Record<string, string>;
  onChange: (dimId: string, code: string) => void;
  /** Stack the selectors (for a left/right placement). */
  vertical?: boolean;
}) {
  // dims is already just the context selectors (server excludes the plotted
  // dimension and any hidden members — see chart.go's buildContextDims).
  if (dims.length === 0) return null;
  return (
    <div style={{ display: "flex", flexWrap: vertical ? "nowrap" : "wrap", flexDirection: vertical ? "column" : "row", gap: 8, alignItems: vertical ? "flex-start" : "center", padding: "4px 0" }}>
      {dims.map(dim => (
        <div key={dim.id} style={{ display: "flex", alignItems: "center", gap: 4 }}>
          <label htmlFor={`ctx-${dim.id}`} style={{ fontSize: 12, color: "var(--color-text-muted)", fontWeight: 500, whiteSpace: "nowrap" }}>
            {dim.name}:
          </label>
          <HierarchicalMemberSelect
            id={`ctx-${dim.id}`}
            ariaLabel={dim.name}
            members={dim.members}
            value={context[dim.id] ?? defaultLeafCode({ members: dim.members }) ?? ""}
            onChange={code => onChange(dim.id, code)}
          />
        </div>
      ))}
    </div>
  );
}

// ── Empty / error states ──────────────────────────────────────────────────────

function EmptyState({ message }: { message: string }) {
  return (
    <div style={{ display: "flex", alignItems: "center", justifyContent: "center", height: "100%", color: "var(--color-text-muted)", fontSize: 13, textAlign: "center", padding: 16 }}>
      {message}
    </div>
  );
}

// ── Individual chart renderers ────────────────────────────────────────────────

// Recharts hands chart-level clicks the hovered category index; tolerate
// the string form some versions emit.
function tooltipIndex(state: unknown): number | null {
  const raw = (state as { activeTooltipIndex?: number | string } | null)?.activeTooltipIndex;
  const i = typeof raw === "string" ? parseInt(raw, 10) : raw;
  return typeof i === "number" && Number.isInteger(i) ? i : null;
}

type CategoryClick = ((index: number) => void) | undefined;

function BarChartRenderer({ data, config, currencySymbol, onCategoryClick }: { data: CategoryChartData; config: GridChartConfig; currencySymbol: string; onCategoryClick?: CategoryClick }) {
  const fmt = makeAxisFormatter(config.value_format, currencySymbol);
  const showLegend = config.show_legend ?? data.series.length > 1;
  const chartData = data.categories.map((cat, i) => {
    const row: Record<string, unknown> = { name: cat.label };
    data.series.forEach(s => { row[s.label] = s.values[i] ?? null; });
    return row;
  });
  return (
    <ResponsiveContainer width="100%" height="100%">
      <BarChart data={chartData} margin={{ top: 8, right: 16, left: 8, bottom: 8 }}
        style={{ cursor: onCategoryClick ? "pointer" : undefined }}
        onClick={onCategoryClick ? (state) => { const i = tooltipIndex(state); if (i !== null) onCategoryClick(i); } : undefined}>
        <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
        <XAxis dataKey="name" tick={{ fontSize: 11 }} />
        <YAxis tickFormatter={fmt} tick={{ fontSize: 11 }} />
        <Tooltip formatter={(v: unknown) => formatValue(v as number, config.value_format, currencySymbol)} />
        {showLegend && <Legend wrapperStyle={{ fontSize: 12 }} />}
        {data.series.map((s, i) => (
          <Bar key={s.metric_id} dataKey={s.label} fill={CHART_COLORS[i % CHART_COLORS.length]}>
            {config.show_values && (
              // eslint-disable-next-line @typescript-eslint/no-explicit-any
              <LabelList dataKey={s.label} position="top" formatter={fmt as any} style={{ fontSize: 10 }} />
            )}
          </Bar>
        ))}
      </BarChart>
    </ResponsiveContainer>
  );
}

function LineChartRenderer({ data, config, currencySymbol, onCategoryClick }: { data: CategoryChartData; config: GridChartConfig; currencySymbol: string; onCategoryClick?: CategoryClick }) {
  const fmt = makeAxisFormatter(config.value_format, currencySymbol);
  const showDots = data.categories.length <= 24;
  const showLegend = config.show_legend ?? data.series.length > 1;
  const chartData = data.categories.map((cat, i) => {
    const row: Record<string, unknown> = { name: cat.label };
    data.series.forEach(s => {
      const v = s.values[i];
      row[s.label] = v === null ? undefined : v;
    });
    return row;
  });
  return (
    <ResponsiveContainer width="100%" height="100%">
      <LineChart data={chartData} margin={{ top: 8, right: 16, left: 8, bottom: 8 }}
        style={{ cursor: onCategoryClick ? "pointer" : undefined }}
        onClick={onCategoryClick ? (state) => { const i = tooltipIndex(state); if (i !== null) onCategoryClick(i); } : undefined}>
        <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
        <XAxis dataKey="name" tick={{ fontSize: 11 }} />
        <YAxis tickFormatter={fmt} tick={{ fontSize: 11 }} />
        <Tooltip formatter={(v: unknown) => formatValue(v as number, config.value_format, currencySymbol)} />
        {showLegend && <Legend wrapperStyle={{ fontSize: 12 }} />}
        {data.series.map((s, i) => (
          <Line
            key={s.metric_id}
            type="linear"
            dataKey={s.label}
            stroke={CHART_COLORS[i % CHART_COLORS.length]}
            dot={showDots}
            connectNulls={false}
          />
        ))}
      </LineChart>
    </ResponsiveContainer>
  );
}

function PieChartRenderer({ data, config, currencySymbol, onCategoryClick }: { data: CategoryChartData; config: GridChartConfig; currencySymbol: string; onCategoryClick?: CategoryClick }) {
  const series = data.series[0];
  if (!series) return <EmptyState message="No data for the selected context." />;

  const hasNegative = data.categories.some((_, i) => (series.values[i] ?? 0) < 0);
  if (hasNegative) {
    return <EmptyState message="Pie charts require non-negative values. Use a bar chart for this metric." />;
  }

  const raw = data.categories
    .map((cat, i) => ({ name: cat.label, value: series.values[i] ?? 0, idx: i as number | undefined }))
    .filter(d => d.value > 0);

  if (raw.length === 0) return <EmptyState message="No data for the selected context." />;

  let slices = raw;
  if (raw.length > 12) {
    const sorted = [...raw].sort((a, b) => b.value - a.value);
    const top11 = sorted.slice(0, 11);
    const otherTotal = sorted.slice(11).reduce((sum, d) => sum + d.value, 0);
    slices = [...top11, { name: "Other", value: otherTotal, idx: undefined }];
  }

  const total = slices.reduce((sum, d) => sum + d.value, 0);
  const showLegend = config.show_legend ?? true;

  return (
    <ResponsiveContainer width="100%" height="100%">
      <PieChart style={{ cursor: onCategoryClick ? "pointer" : undefined }}>
        <Pie
          onClick={onCategoryClick ? (_, i) => { const idx = slices[i]?.idx; if (typeof idx === "number") onCategoryClick(idx); } : undefined}
          data={slices}
          dataKey="value"
          nameKey="name"
          cx="50%"
          cy="50%"
          outerRadius="70%"
        >
          {slices.map((_, i) => (
            <Cell key={i} fill={CHART_COLORS[i % CHART_COLORS.length]} />
          ))}
        </Pie>
        <Tooltip
          formatter={((v: unknown, name: unknown) => {
            const val = v as number;
            return [
              `${formatValue(val, config.value_format, currencySymbol)} (${total > 0 ? ((val / total) * 100).toFixed(1) : 0}%)`,
              name,
            ];
          // eslint-disable-next-line @typescript-eslint/no-explicit-any
          }) as any}
        />
        {showLegend && <Legend wrapperStyle={{ fontSize: 12 }} />}
      </PieChart>
    </ResponsiveContainer>
  );
}

function ScatterChartRenderer({ data, config, currencySymbol }: { data: ScatterChartData; config: GridChartConfig; currencySymbol: string }) {
  const fmt = makeAxisFormatter(config.value_format, currencySymbol);
  return (
    <ResponsiveContainer width="100%" height="100%">
      <ScatterChart margin={{ top: 8, right: 16, left: 8, bottom: 24 }}>
        <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" />
        <XAxis
          dataKey="x"
          name={data.x_metric.label}
          tickFormatter={fmt}
          type="number"
          tick={{ fontSize: 11 }}
          label={{ value: data.x_metric.label, position: "insideBottom", offset: -12, fontSize: 11 }}
        />
        <YAxis
          dataKey="y"
          name={data.y_metric.label}
          tickFormatter={fmt}
          type="number"
          tick={{ fontSize: 11 }}
          label={{ value: data.y_metric.label, angle: -90, position: "insideLeft", fontSize: 11 }}
        />
        <Tooltip
          content={({ payload }) => {
            if (!payload?.length) return null;
            const p = payload[0].payload as { label: string; x: number; y: number };
            return (
              <div style={{ background: "var(--color-surface)", border: "1px solid var(--color-border)", borderRadius: 6, padding: "8px 12px", fontSize: 12 }}>
                <div style={{ fontWeight: 600, marginBottom: 4 }}>{p.label}</div>
                <div>{data.x_metric.label}: {formatValue(p.x, config.value_format, currencySymbol)}</div>
                <div>{data.y_metric.label}: {formatValue(p.y, config.value_format, currencySymbol)}</div>
              </div>
            );
          }}
        />
        <Scatter data={data.points} fill={CHART_COLORS[0]} />
      </ScatterChart>
    </ResponsiveContainer>
  );
}

function HistogramRenderer({ data, config, currencySymbol }: { data: HistogramChartData; config: GridChartConfig; currencySymbol: string }) {
  const chartData = data.bins.map(b => ({
    range: `${formatValue(b.min, config.value_format, currencySymbol)}–${formatValue(b.max, config.value_format, currencySymbol)}`,
    count: b.count,
  }));
  return (
    <div style={{ display: "flex", flexDirection: "column", height: "100%", gap: 4 }}>
      <div style={{ fontSize: 11, color: "var(--color-text-muted)", textAlign: "center" }}>
        {data.observation_count} observations · {data.metric.label}
      </div>
      <div style={{ flex: 1, minHeight: 0 }}>
        <ResponsiveContainer width="100%" height="100%">
          <BarChart data={chartData} barCategoryGap={0} margin={{ top: 4, right: 16, left: 8, bottom: 8 }}>
            <CartesianGrid strokeDasharray="3 3" stroke="var(--color-border)" vertical={false} />
            <XAxis dataKey="range" tick={{ fontSize: 10 }} interval="preserveStartEnd" />
            <YAxis
              allowDecimals={false}
              tick={{ fontSize: 11 }}
              label={{ value: "Count", angle: -90, position: "insideLeft", fontSize: 11 }}
            />
            <Tooltip formatter={(v: unknown) => [v as number, "Count"]} labelFormatter={(l: unknown) => `Range: ${l}`} />
            <Bar dataKey="count" fill={CHART_COLORS[0]} />
          </BarChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}

function ChartBody({ data, config, currencySymbol, onCategoryClick }: { data: ChartData; config: GridChartConfig; currencySymbol: string; onCategoryClick?: CategoryClick }) {
  switch (data.chart_type) {
    case "bar":
      return <BarChartRenderer data={data as CategoryChartData} config={config} currencySymbol={currencySymbol} onCategoryClick={onCategoryClick} />;
    case "line":
      return <LineChartRenderer data={data as CategoryChartData} config={config} currencySymbol={currencySymbol} onCategoryClick={onCategoryClick} />;
    case "pie":
      return <PieChartRenderer data={data as CategoryChartData} config={config} currencySymbol={currencySymbol} onCategoryClick={onCategoryClick} />;
    case "scatter":
      return <ScatterChartRenderer data={data as ScatterChartData} config={config} currencySymbol={currencySymbol} />;
    case "histogram":
      return <HistogramRenderer data={data as HistogramChartData} config={config} currencySymbol={currencySymbol} />;
    default:
      return <EmptyState message="Unknown chart type." />;
  }
}

// format_currency is a literal symbol per metric (e.g. "$", "€"), not a
// single app-wide setting — a chart can plot several metrics at once, so
// there's no one "correct" symbol in general; using the first plotted
// metric's own symbol at least ties the axis/tooltip formatting to a real
// per-metric setting instead of a value picked with no connection to any
// metric in the model.
function firstMetricFormatCurrency(data: ChartData | undefined): string {
  if (!data) return "";
  switch (data.chart_type) {
    case "bar":
    case "line":
    case "pie":
      return data.series[0]?.format_currency ?? "";
    case "scatter":
      return data.x_metric.format_currency;
    case "histogram":
      return data.metric.format_currency;
    default:
      return "";
  }
}

// ── Main ChartWidget ──────────────────────────────────────────────────────────

export function ChartWidget({ widget, ctx }: { widget: DashboardWidget; ctx: DemoContext }) {
  const chartConfig = widget.widget_props?.chart;

  const [context, setContext] = useState<Record<string, string>>(
    chartConfig?.context_defaults ?? {}
  );

  // Reset context when the plotted dimension changes
  const configKey = `${widget.id}:${chartConfig?.dimension_id}`;
  const prevConfigKeyRef = React.useRef(configKey);
  useEffect(() => {
    if (prevConfigKeyRef.current !== configKey) {
      prevConfigKeyRef.current = configKey;
      setContext(chartConfig?.context_defaults ?? {});
    }
  }, [configKey, chartConfig?.context_defaults]);

  const refreshMs = (chartConfig?.refresh_seconds ?? 2) * 1000;

  // Fetch chart data — resolved and rolled up server-side (internal/rollup
  // via chart.go), matching the grid exactly instead of a second client-side
  // computation over raw grid cells.
  const { data: chartData, isPending, isError } = useQuery({
    queryKey: ["chart-data", ctx.revision_id, widget.id, context],
    queryFn: () => api.getChartData(widget.id, context),
    enabled: !!chartConfig && isChartConfigComplete(chartConfig),
    staleTime: 0,
    refetchInterval: refreshMs,
    refetchOnWindowFocus: true,
    placeholderData: (prev) => prev,
  });

  const currencySymbol = firstMetricFormatCurrency(chartData) || "$";

  // Must be called unconditionally on every render (rules of hooks), so
  // this sits before the early-return guard below — dims/context/setContext
  // are all already available at this point regardless of chartConfig.
  const dims: ChartContextDim[] = chartData?.context_dims ?? [];
  // Sync is ON unless the widget explicitly opts out — a dashboard's
  // widgets sharing one context (and one set of selectors) is the norm.
  const syncOn = widget.widget_props?.sync_context !== false;
  const setContextValue = useWidgetContextSync(dims, syncOn, context, setContext);
  const ownsSelector = useSelectorOwnership(syncOn);
  // The server resolves for a VISIBLE member when the requested one is hidden
  // from this viewer (a developer's context_defaults pin, say) and reports
  // the context it used. Adopt it — through the sync setter, so the shared
  // selectors and every widget following them show the member the chart
  // really shows instead of the hidden one. Converges in one round trip:
  // the adopted value is visible, so the next response echoes it unchanged.
  // A dimension the dashboard's shared pool already has a value for is left
  // alone: that value is visible by construction and useWidgetContextSync
  // adopts it here in the same render, so pushing the server's pick would
  // only override what every other widget already agreed on.
  const effectiveContext = chartData?.context;
  const sharedValues = useContext(DashboardContextSyncContext)?.values;
  useEffect(() => {
    if (!effectiveContext) return;
    for (const [dimId, code] of Object.entries(effectiveContext)) {
      if (syncOn && sharedValues?.[dimId] !== undefined) continue;
      if (code && context[dimId] !== undefined && context[dimId] !== code && dims.some(d => d.id === dimId)) {
        setContextValue(dimId, code);
      }
    }
    // Only when a new payload arrives — `context` changes right after (that's
    // the adoption), and re-running then would be a no-op anyway.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [effectiveContext]);
  // Click-to-select: a bar/point/slice pushes its category (a member of the
  // plotted dimension) into the dashboard-wide context, so every synced
  // widget selecting on that dimension follows — click EMEA, see EMEA. This
  // is the user's explicit choice, so it works on any dashboard, even when
  // this chart's OWN selectors do not follow the shared context
  // (sync_context=false) — reported live: "click in region doesn't sync".
  const pushSync = useSyncSetter(true);
  const categories = chartData && "categories" in chartData ? (chartData as CategoryChartData).categories : undefined;
  const plottedDimId = chartConfig?.dimension_id;
  const onCategoryClick: CategoryClick = pushSync && plottedDimId && categories
    ? (i) => { const key = categories[i]?.key; if (key) pushSync(plottedDimId, key); }
    : undefined;
  // Only render selectors this widget owns — another widget on the same
  // dashboard already shows the picker for the rest (values stay synced).
  const selectorDims = dims.filter(d => ownsSelector(d.id));

  if (!chartConfig || !isChartConfigComplete(chartConfig)) {
    return <EmptyState message="Chart configuration is incomplete. Open the dashboard editor and set the dimension and metrics." />;
  }

  const hasData = !!chartData;
  const isEmpty = hasData && (
    (chartData.chart_type === "bar" || chartData.chart_type === "line" || chartData.chart_type === "pie")
      ? (chartData as CategoryChartData).categories.length === 0
      : chartData.chart_type === "scatter"
        ? (chartData as ScatterChartData).points.length === 0
        : (chartData as HistogramChartData).bins.length === 0
  );

  const chartLabel = widget.title || CHART_TYPE_LABELS[chartConfig.chart_type];
  // Developer-chosen selector placement (widget_props.selectors_position).
  const selPos = widget.widget_props?.selectors_position ?? "top";
  const selVertical = selPos === "left" || selPos === "right";

  return (
    <div
      style={{ display: "flex", flexDirection: selPos === "left" ? "row" : selPos === "right" ? "row-reverse" : "column", height: "100%", padding: "8px 12px", gap: selVertical ? 12 : 6 }}
      role="figure"
      aria-label={chartLabel}
    >
      {/* Context selectors — ContextSelectors itself already no-ops when
          there are no non-plotted dims (contextDims.length === 0); gating
          on `context` being non-empty here too was wrong and hid the
          control precisely in the common case that needs it most: no
          `context_defaults` configured, so every non-plotted dim silently
          defaults to whatever `dim.members[0]` happens to be (e.g.
          alphabetically first, not necessarily a member with any data) —
          with no way to change it, since this row never rendered. */}
      {selectorDims.length > 0 && (
        <div style={{ order: selPos === "bottom" ? 2 : 0, flexShrink: 0 }}>
          <ContextSelectors
            dims={selectorDims}
            context={context}
            onChange={setContextValue}
            vertical={selVertical}
          />
        </div>
      )}

      {/* Chart body */}
      <div style={{ flex: 1, minHeight: 0, position: "relative" }}>
        {isPending && !chartData && (
          <EmptyState message="Loading chart…" />
        )}
        {isError && (
          <EmptyState message="Unable to load chart data. Retry." />
        )}
        {!isError && isEmpty && <EmptyState message="No data for the selected context." />}
        {hasData && !isEmpty && !isError && (
          <ChartBody data={chartData} config={chartConfig} currencySymbol={currencySymbol} onCategoryClick={onCategoryClick} />
        )}
      </div>

      {/* Accessible data summary */}
      {hasData && (
        <p className="sr-only" aria-live="polite">
          {chartLabel}, {CHART_TYPE_LABELS[chartConfig.chart_type]}.
          Context: {Object.entries(context).map(([k, v]) => `${dims.find(d => d.id === k)?.name ?? k}=${v}`).join(", ")}.
          {onCategoryClick && " Click a category to select it in synced widgets."}
        </p>
      )}
    </div>
  );
}

