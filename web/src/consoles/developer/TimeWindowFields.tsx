import type { TimeWindow } from "../../api/client";
import { Checkbox, Field, NumberInput, Select } from "../../ui";

/**
 * A grid or chart widget's rolling window (widget_props.time_window): its time
 * dimension shown from the period holding the date a metric holds — the first
 * forecast week, the last closed month — for a number of periods, some
 * before it if wanted. When the date moves, the window moves. Members ticked
 * under the dimension's members shown (a total) follow the window.
 */
export function TimeWindowFields({ timeDims, dateMetrics, value, onChange }: {
  timeDims: { id: string; label: string }[];
  dateMetrics: { id: string; label: string }[];
  value: TimeWindow | undefined;
  onChange: (v: TimeWindow | undefined) => void;
}) {
  if (timeDims.length === 0) return null;
  const on = !!value;
  const v: TimeWindow = value ?? { dimension_id: timeDims[0].id, from_metric_id: dateMetrics[0]?.id ?? "", periods: 13 };
  const set = (p: Partial<TimeWindow>) => onChange({ ...v, ...p });
  return (
    <div style={{ marginBottom: 12 }}>
      <div className="mvx-prop-section">Rolling window</div>
      <Checkbox checked={on} disabled={!on && dateMetrics.length === 0} label="Show periods from a date a metric holds"
        onChange={e => onChange(e.target.checked ? v : undefined)} />
      <p style={{ fontSize: 11, color: "var(--color-text-quiet)", margin: "2px 0 6px" }}>
        {dateMetrics.length === 0 && !on
          ? "Needs a date metric without dimensions (a setting such as the first forecast week)."
          : "The window moves when the date does. Members ticked under the dimension's members shown (a total) follow it."}
      </p>
      {on && (
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 8 }}>
          <Field label="Time dimension">
            <Select value={v.dimension_id} aria-label="Window time dimension" onChange={e => set({ dimension_id: e.target.value })}>
              {timeDims.map(d => <option key={d.id} value={d.id}>{d.label}</option>)}
            </Select>
          </Field>
          <Field label="From the date in">
            <Select value={v.from_metric_id} aria-label="Window start date metric" onChange={e => set({ from_metric_id: e.target.value })}>
              {dateMetrics.map(m => <option key={m.id} value={m.id}>{m.label}</option>)}
            </Select>
          </Field>
          <Field label="Periods">
            <NumberInput min={1} max={400} value={v.periods} aria-label="Window periods"
              onChange={e => set({ periods: Math.max(1, Math.min(400, parseInt(e.target.value) || 1)) })} />
          </Field>
          <Field label="Periods before it">
            <NumberInput min={0} max={400} value={v.before ?? 0} aria-label="Window periods before"
              onChange={e => set({ before: Math.max(0, Math.min(400, parseInt(e.target.value) || 0)) || undefined })} />
          </Field>
        </div>
      )}
    </div>
  );
}
