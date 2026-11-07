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
      // The sign before the symbol (-$360K), as the KPI tile and the grid
      // write it — not $-360K.
      return `${value < 0 ? "-" : ""}${currencySymbol}${new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 2 }).format(Math.abs(value))}`;
    case "percent":
      // Percent units, as Percentage metrics are stored (5.6 = 5.6%) and the
      // grid and KPI tiles show them — not a fraction to multiply by 100.
      return `${new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value)}%`;
    case "compact":
      return new Intl.NumberFormat(undefined, { notation: "compact", maximumFractionDigits: 2 }).format(value);
    default:
      return new Intl.NumberFormat(undefined, { maximumFractionDigits: 2 }).format(value);
  }
}

// niceTicks are round axis ticks — 1, 2, 2.5 or 5 times a power of ten —
// from at or below min to at or above max. The axis's own ticks were not
// round and mixed units (950K, 1.9M, 2.85M, 3.8M).
export function niceTicks(min: number, max: number, target = 5): number[] {
  if (!Number.isFinite(min) || !Number.isFinite(max)) return [];
  if (min > max) [min, max] = [max, min];
  if (min === max) {
    if (min === 0) return [0, 1];
    min = Math.min(0, min);
    max = Math.max(0, max);
  }
  const raw = (max - min) / target;
  const pow = Math.pow(10, Math.floor(Math.log10(raw)));
  const step = [1, 2, 2.5, 5, 10].map(m => m * pow).find(st => st >= raw - 1e-12) ?? 10 * pow;
  const lo = Math.floor(min / step + 1e-9) * step;
  const hi = Math.ceil(max / step - 1e-9) * step;
  const out: number[] = [];
  for (let v = lo; v <= hi + step / 2; v += step) out.push(Number(v.toPrecision(12)));
  return out;
}

// axisTickFormatter writes every tick of an axis in one unit — the largest
// tick's (K, M or B) — with the decimals the step needs: 0.5M, 1.0M, 1.5M.
export function axisTickFormatter(ticks: number[], format: string | undefined, currencySymbol = "$"): (v: number) => string {
  const maxAbs = Math.max(0, ...ticks.map(t => Math.abs(t)));
  const [div, suffix] = format === "percent" ? [1, "%"]
    : maxAbs >= 1e9 ? [1e9, "B"] : maxAbs >= 1e6 ? [1e6, "M"] : maxAbs >= 1e3 ? [1e3, "K"] : [1, ""];
  const step = ticks.length > 1 ? Math.abs(ticks[1] - ticks[0]) / div : 1;
  const frac = step.toFixed(6).replace(/0+$/, "").split(".")[1] ?? "";
  const nf = new Intl.NumberFormat(undefined, { minimumFractionDigits: frac.length, maximumFractionDigits: frac.length });
  return (v: number) => {
    const n = nf.format(Math.abs(v) / div);
    const sign = v < 0 ? "-" : "";
    return format === "currency" ? `${sign}${currencySymbol}${n}${suffix}` : `${sign}${n}${suffix}`;
  };
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
