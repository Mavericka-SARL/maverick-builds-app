import type { GridChartConfig, GridChartType } from "../../api/client";

// Series colours are design tokens so they follow the light/dark theme.
export const CHART_COLORS = Array.from({ length: 10 }, (_, i) => `var(--color-chart-${i + 1})`);

// currencySymbol is a literal prefix (e.g. "$", "€"), not an ISO 4217 code —
// metric_def.format_currency stores whatever symbol a developer typed in
// (see MetricRow's "Symbol" field in DeveloperConsole.tsx), so this can't
// use Intl.NumberFormat's `currency` style, which only accepts real ISO
// codes. Defaults to "$" to match every other currency-formatting call
// site in the app (MetricKpiWidget, the import template builder) when no
// metric-specific symbol is available.
export function formatValue(value: number | null | undefined, format: string | undefined, currencySymbol = "$"): string {
  if (value === null || value === undefined) return "—";
  switch (format) {
    case "currency":
      return `${currencySymbol}${new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 2 }).format(value)}`;
    case "percent":
      return new Intl.NumberFormat(undefined, { style: "percent", maximumFractionDigits: 2 }).format(value);
    case "compact":
      return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 2 }).format(value);
    default:
      return new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value);
  }
}

export const CHART_TYPE_LABELS: Record<GridChartType, string> = {
  bar: "Bar Chart",
  line: "Line Chart",
  pie: "Pie Chart",
  scatter: "Scatter Plot",
  histogram: "Histogram",
};

export function isChartConfigComplete(cfg: Partial<GridChartConfig>): boolean {
  if (!cfg.chart_type || !cfg.dimension_id) return false;
  if (cfg.chart_type === "scatter") {
    return !!(cfg.x_metric_id && cfg.y_metric_id && cfg.x_metric_id !== cfg.y_metric_id);
  }
  return (cfg.metric_ids?.length ?? 0) >= 1;
}

export function chartConfigFromDraft(draft: Partial<GridChartConfig>): GridChartConfig | null {
  if (!isChartConfigComplete(draft)) return null;
  return {
    chart_type: draft.chart_type!,
    dimension_id: draft.dimension_id!,
    metric_ids: draft.metric_ids ?? [],
    x_metric_id: draft.x_metric_id,
    y_metric_id: draft.y_metric_id,
    context_defaults: draft.context_defaults ?? {},
    bin_count: draft.bin_count,
    show_legend: draft.show_legend,
    show_values: draft.show_values,
    value_format: draft.value_format,
    refresh_seconds: draft.refresh_seconds,
    hide_rollup_members: draft.hide_rollup_members,
  };
}
