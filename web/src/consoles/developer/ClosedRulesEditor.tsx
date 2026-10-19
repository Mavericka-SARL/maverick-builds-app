import { Plus, X as XIcon } from "lucide-react";
import type { ClosedRule } from "../../api/client";
import { Button, IconButton, Select, TextInput } from "../../ui";

const OPS: ClosedRule["op"][] = ["=", "<>", ">", ">=", "<", "<="];

// The cells of an input metric that take no input (metric.closed_when): a
// calculated metric of the same grid compared with a number. A cell where
// any rule holds is shown read-only and every write refuses it.
export function ClosedRulesEditor({ rules, onChange, metricNames }: {
  rules: ClosedRule[];
  onChange: (rules: ClosedRule[]) => void;
  /** The calculated metrics a rule may read (those of the same grid). */
  metricNames: string[];
}) {
  const set = (i: number, patch: Partial<ClosedRule>) => onChange(rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {rules.map((r, i) => (
        <div key={i} style={{ display: "flex", gap: 6, alignItems: "center" }}>
          <span className="mvx-admin-muted">closed where</span>
          <Select value={r.metric} onChange={e => set(i, { metric: e.target.value })} aria-label={`Closed rule ${i + 1} metric`}>
            {!metricNames.includes(r.metric) && <option value={r.metric}>{r.metric || "(choose a metric)"}</option>}
            {metricNames.map(n => <option key={n} value={n}>{n}</option>)}
          </Select>
          <Select value={r.op} onChange={e => set(i, { op: e.target.value as ClosedRule["op"] })} aria-label={`Closed rule ${i + 1} comparison`}>
            {OPS.map(o => <option key={o} value={o}>{o}</option>)}
          </Select>
          <TextInput type="number" value={String(r.value)} style={{ width: 80 }} aria-label={`Closed rule ${i + 1} value`}
            onChange={e => set(i, { value: Number(e.target.value) })} />
          <IconButton aria-label={`Remove closed rule ${i + 1}`} title="Remove" size={26} onClick={() => onChange(rules.filter((_, j) => j !== i))}>
            <XIcon size={13} />
          </IconButton>
        </div>
      ))}
      <div>
        <Button size="sm" leadingIcon={<Plus size={13} />} disabled={metricNames.length === 0}
          onClick={() => onChange([...rules, { metric: metricNames[0] ?? "", op: "=", value: 1 }])}>
          Add a closed rule
        </Button>
      </div>
    </div>
  );
}
