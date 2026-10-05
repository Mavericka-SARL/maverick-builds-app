import { Plus, X } from "lucide-react";
import type { HighlightRule, HighlightTone } from "../../api/client";
import { Button, IconButton, Select, TextInput } from "../../ui";

// The developer's editor for a metric's highlight rules (conditional
// highlighting): each rule compares the cell — or another metric at the
// same cell — with a number, a text or another metric, and tints it. The
// server checks them (metricformula.CheckHighlightRules); this keeps the
// shape right while typing.

const OPS: { value: HighlightRule["op"]; label: string }[] = [
  { value: ">", label: ">" }, { value: ">=", label: "≥" }, { value: "<", label: "<" }, { value: "<=", label: "≤" },
  { value: "=", label: "=" }, { value: "<>", label: "≠" }, { value: "between", label: "between" },
  { value: "not_between", label: "not between" }, { value: "blank", label: "is blank" }, { value: "not_blank", label: "is not blank" },
];
const TONES: { value: HighlightTone; label: string }[] = [
  { value: "negative", label: "Red" }, { value: "warning", label: "Amber" }, { value: "positive", label: "Green" }, { value: "info", label: "Blue" },
];

export function HighlightRulesEditor({ rules, onChange, metricNames }: {
  rules: HighlightRule[];
  onChange: (rules: HighlightRule[]) => void;
  metricNames: string[];
}) {
  const set = (i: number, patch: Partial<HighlightRule>) =>
    onChange(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {rules.map((r, i) => {
        const noValue = r.op === "blank" || r.op === "not_blank";
        const range = r.op === "between" || r.op === "not_between";
        const against = r.than !== undefined ? "metric" : "value";
        return (
          <div key={i} style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }} aria-label={`Highlight rule ${i + 1}`}>
            <span className="mvx-admin-muted">When</span>
            <Select aria-label="Tests" value={r.metric ?? ""} onChange={e => set(i, { metric: e.target.value || undefined })} style={{ width: 150 }}>
              <option value="">this cell</option>
              {metricNames.map(n => <option key={n} value={n}>{n}</option>)}
            </Select>
            <label style={{ display: "inline-flex", gap: 4, alignItems: "center" }} className="mvx-admin-muted">
              <input type="checkbox" checked={!!r.abs} onChange={e => set(i, { abs: e.target.checked || undefined })} /> absolute
            </label>
            <Select aria-label="Comparison" value={r.op} onChange={e => {
              const op = e.target.value as HighlightRule["op"];
              const patch: Partial<HighlightRule> = { op };
              if (op === "blank" || op === "not_blank") Object.assign(patch, { value: undefined, value2: undefined, than: undefined });
              set(i, patch);
            }} style={{ width: 110 }}>
              {OPS.map(o => <option key={o.value} value={o.value}>{o.label}</option>)}
            </Select>
            {!noValue && !range && (
              <Select aria-label="Compared with" value={against} onChange={e => set(i, e.target.value === "metric"
                ? { than: metricNames[0] ?? "", value: undefined }
                : { than: undefined, value: 0 })} style={{ width: 100 }}>
                <option value="value">a value</option>
                <option value="metric">a metric</option>
              </Select>
            )}
            {!noValue && against === "metric" && !range ? (
              <Select aria-label="Metric compared with" value={r.than ?? ""} onChange={e => set(i, { than: e.target.value })} style={{ width: 160 }}>
                {metricNames.map(n => <option key={n} value={n}>{n}</option>)}
              </Select>
            ) : !noValue && (
              <TextInput aria-label="Value" value={r.value === undefined ? "" : String(r.value)} style={{ width: 90 }}
                onChange={e => {
                  const t = e.target.value;
                  const n = Number(t);
                  set(i, { value: t.trim() !== "" && !isNaN(n) ? n : t });
                }} />
            )}
            {range && (
              <>
                <span className="mvx-admin-muted">and</span>
                <TextInput aria-label="Upper value" value={r.value2 === undefined ? "" : String(r.value2)} style={{ width: 90 }}
                  onChange={e => set(i, { value2: Number(e.target.value) })} />
              </>
            )}
            <span className="mvx-admin-muted">→</span>
            <Select aria-label="Tone" value={r.tone} onChange={e => set(i, { tone: e.target.value as HighlightTone })} style={{ width: 90 }}>
              {TONES.map(t => <option key={t.value} value={t.value}>{t.label}</option>)}
            </Select>
            <IconButton aria-label={`Remove highlight rule ${i + 1}`} size={24} onClick={() => onChange(rules.filter((_, j) => j !== i))}>
              <X size={13} />
            </IconButton>
          </div>
        );
      })}
      <div>
        <Button size="sm" variant="ghost" onClick={() => onChange([...rules, { op: ">", value: 0, tone: "negative" }])}>
          <Plus size={13} /> Add highlight rule
        </Button>
      </div>
    </div>
  );
}
