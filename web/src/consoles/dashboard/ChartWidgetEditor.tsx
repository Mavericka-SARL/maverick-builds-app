import React from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type GridChartConfig, type GridChartType, type DemoContext } from "../../api/client";
import { CHART_TYPE_LABELS } from "./chartTypes";
import { defaultLeafCode } from "../dashboardLayout";
import { HierarchicalMemberSelect } from "../HierarchicalMemberSelect";

const inputS: React.CSSProperties = {
  border: "1px solid #d1d5db", borderRadius: 4, padding: "4px 8px",
  fontSize: 12, width: "100%", background: "#fff",
};
const lbl: React.CSSProperties = {
  display: "block", fontSize: 10, color: "#9ca3af", marginBottom: 2, fontWeight: 600,
};

type ChartDraft = Partial<GridChartConfig>;

interface ChartWidgetEditorProps {
  gridDefId: string;
  draft: ChartDraft;
  ctx: DemoContext;
  onChange: (patch: ChartDraft) => void;
}

const CHART_TYPES: GridChartType[] = ["bar", "line", "pie", "scatter", "histogram"];
const VALUE_FORMATS = ["number", "currency", "percent", "compact"] as const;
const REFRESH_OPTIONS = [2, 5, 10, 30] as const;

export function ChartWidgetEditor({ gridDefId, draft, ctx, onChange }: ChartWidgetEditorProps) {
  const [truncationNotice, setTruncationNotice] = React.useState<string | null>(null);
  const { data: gridData } = useQuery({
    queryKey: ["grid", ctx.revision_id, gridDefId],
    queryFn: () => api.getGrid(ctx.revision_id, gridDefId),
    enabled: !!gridDefId && !!ctx.revision_id,
    staleTime: 60_000,
  });

  const dims = gridData?.dimensions ?? [];
  const metrics = gridData?.metrics ?? [];

  const chartType: GridChartType = draft.chart_type ?? "bar";
  const dimensionId = draft.dimension_id ?? "";
  const metricIds = draft.metric_ids ?? [];
  const xMetricId = draft.x_metric_id ?? "";
  const yMetricId = draft.y_metric_id ?? "";
  const contextDefaults = draft.context_defaults ?? {};

  const isBar = chartType === "bar" || chartType === "line";
  const isPie = chartType === "pie" || chartType === "histogram";
  const isScatter = chartType === "scatter";

  // Context dimensions = all dims except the plotted one
  const contextDims = dims.filter(d => d.id !== dimensionId);

  function handleChartTypeChange(ct: GridChartType) {
    const patch: ChartDraft = { chart_type: ct };
    // Reset metric ids to valid counts
    let dropped = 0;
    if (ct === "scatter") {
      patch.metric_ids = [];
      patch.x_metric_id = metricIds[0] ?? "";
      patch.y_metric_id = metricIds[1] ?? "";
      dropped = Math.max(0, metricIds.length - 2);
    } else if (ct === "pie" || ct === "histogram") {
      patch.metric_ids = metricIds.slice(0, 1);
      dropped = Math.max(0, metricIds.length - 1);
    } else {
      patch.metric_ids = metricIds.slice(0, 5);
    }
    setTruncationNotice(
      dropped > 0
        ? `Switched to ${CHART_TYPE_LABELS[ct]} — dropped ${dropped} metric${dropped === 1 ? "" : "s"} it can't plot. Reselect below if needed.`
        : null
    );
    onChange(patch);
  }

  function handleDimChange(dimId: string) {
    const newContextDefaults: Record<string, string> = {};
    // Remove old plotted dim from context, add previous plotted to context if it exists
    dims.forEach(d => {
      if (d.id === dimId) return; // new plotted dim — not in context
      if (d.id === dimensionId) {
        // previous plotted dim moves to context with its first real leaf
        const leafCode = defaultLeafCode(d);
        if (leafCode) newContextDefaults[d.id] = leafCode;
      } else {
        // keep existing context selection if available
        newContextDefaults[d.id] = contextDefaults[d.id] ?? defaultLeafCode(d) ?? "";
      }
    });
    onChange({ dimension_id: dimId, context_defaults: newContextDefaults });
  }

  function toggleMetric(metricId: string) {
    const maxMetrics = isBar ? 5 : 1;
    const next = metricIds.includes(metricId)
      ? metricIds.filter(id => id !== metricId)
      : metricIds.length < maxMetrics ? [...metricIds, metricId] : metricIds;
    setTruncationNotice(null);
    onChange({ metric_ids: next });
  }

  function setContextDefault(dimId: string, code: string) {
    onChange({ context_defaults: { ...contextDefaults, [dimId]: code } });
  }

  if (!gridDefId) {
    return <p style={{ fontSize: 12, color: "#9ca3af" }}>Select a grid source first.</p>;
  }

  if (!gridData) {
    return <p style={{ fontSize: 12, color: "#9ca3af" }}>Loading grid…</p>;
  }

  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
      {/* Chart type */}
      <div>
        <label style={lbl}>CHART TYPE</label>
        <select value={chartType} onChange={e => handleChartTypeChange(e.target.value as GridChartType)} style={inputS}>
          {CHART_TYPES.map(ct => (
            <option key={ct} value={ct}>{CHART_TYPE_LABELS[ct]}</option>
          ))}
        </select>
        {truncationNotice && (
          <p style={{ fontSize: 11, color: "var(--color-warning)", marginTop: 4, marginBottom: 0 }}>{truncationNotice}</p>
        )}
      </div>

      {/* Plotted dimension */}
      <div>
        <label style={lbl}>PLOTTED DIMENSION</label>
        <select value={dimensionId} onChange={e => handleDimChange(e.target.value)} style={inputS}>
          <option value="">— select —</option>
          {dims.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
        </select>
      </div>

      {/* Metrics */}
      {(isBar || isPie) && (
        <div>
          <label style={lbl}>
            {isBar ? "METRICS (up to 5)" : "METRIC (exactly 1)"}
          </label>
          <div style={{ display: "flex", flexDirection: "column", gap: 3 }}>
            {metrics.map(m => {
              const checked = metricIds.includes(m.id);
              const disabled = !checked && isBar && metricIds.length >= 5;
              return (
                <label key={m.id} style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, cursor: disabled ? "not-allowed" : "pointer", opacity: disabled ? 0.5 : 1 }}>
                  <input
                    type="checkbox"
                    checked={checked}
                    disabled={disabled}
                    onChange={() => toggleMetric(m.id)}
                  />
                  {m.label || m.name}
                </label>
              );
            })}
          </div>
        </div>
      )}

      {/* Scatter: X and Y metric selectors */}
      {isScatter && (
        <>
          <div>
            <label style={lbl}>X-AXIS METRIC</label>
            <select value={xMetricId} onChange={e => onChange({ x_metric_id: e.target.value })} style={inputS}>
              <option value="">— select —</option>
              {metrics.map(m => <option key={m.id} value={m.id}>{m.label || m.name}</option>)}
            </select>
          </div>
          <div>
            <label style={lbl}>Y-AXIS METRIC</label>
            <select value={yMetricId} onChange={e => onChange({ y_metric_id: e.target.value })} style={inputS}>
              <option value="">— select —</option>
              {metrics.filter(m => m.id !== xMetricId).map(m => <option key={m.id} value={m.id}>{m.label || m.name}</option>)}
            </select>
          </div>
        </>
      )}

      {/* Histogram bin count */}
      {chartType === "histogram" && (
        <div>
          <label style={lbl}>BIN COUNT (3–30)</label>
          <input
            type="number"
            min={3}
            max={30}
            value={draft.bin_count ?? 10}
            onChange={e => onChange({ bin_count: Math.min(30, Math.max(3, Number(e.target.value))) })}
            style={inputS}
          />
        </div>
      )}

      {/* Context defaults */}
      {contextDims.length > 0 && (
        <div>
          <label style={lbl}>CONTEXT DEFAULTS</label>
          <div style={{ display: "flex", flexDirection: "column", gap: 5 }}>
            {contextDims.map(dim => {
              const currentCode = contextDefaults[dim.id] ?? defaultLeafCode(dim) ?? "";
              return (
                <div key={dim.id}>
                  <label style={{ display: "block", fontSize: 10, color: "#6b7280", marginBottom: 2 }}>{dim.name}</label>
                  <HierarchicalMemberSelect
                    ariaLabel={dim.name}
                    members={dim.members}
                    value={currentCode}
                    onChange={code => setContextDefault(dim.id, code)}
                    className="mvx-hier-select__trigger--full-width"
                  />
                </div>
              );
            })}
          </div>
        </div>
      )}

      {/* Display options */}
      <div>
        <label style={lbl}>VALUE FORMAT</label>
        <select value={draft.value_format ?? "number"} onChange={e => onChange({ value_format: e.target.value as "number" | "currency" | "percent" | "compact" })} style={inputS}>
          {VALUE_FORMATS.map(f => <option key={f} value={f}>{f}</option>)}
        </select>
      </div>

      <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, cursor: "pointer" }}>
        <input
          type="checkbox"
          checked={draft.show_legend ?? true}
          onChange={e => onChange({ show_legend: e.target.checked })}
        />
        Show legend
      </label>

      {/* A total is the sum of everything beside it, so plotting "All Regions"
          next to its own regions gives one bar roughly twice the height of the
          others and flattens the comparison. Applies to the plotted axis only —
          the context selectors keep their rollups, where picking "All Regions"
          is a real choice. */}
      <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, cursor: "pointer" }}>
        <input
          type="checkbox"
          checked={draft.hide_rollup_members ?? false}
          onChange={e => onChange({ hide_rollup_members: e.target.checked })}
        />
        Hide totals (e.g. "All Regions")
      </label>

      {(chartType === "bar" || chartType === "line") && (
        <label style={{ display: "flex", alignItems: "center", gap: 6, fontSize: 12, cursor: "pointer" }}>
          <input
            type="checkbox"
            checked={draft.show_values ?? false}
            onChange={e => onChange({ show_values: e.target.checked })}
          />
          Show values on chart
        </label>
      )}

      <div>
        <label style={lbl}>REFRESH INTERVAL</label>
        <select
          value={draft.refresh_seconds ?? 2}
          onChange={e => onChange({ refresh_seconds: Number(e.target.value) as 2 | 5 | 10 | 30 })}
          style={inputS}
        >
          {REFRESH_OPTIONS.map(s => <option key={s} value={s}>{s}s</option>)}
        </select>
      </div>
    </div>
  );
}

