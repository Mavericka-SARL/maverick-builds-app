import { Plus, X as XIcon } from "lucide-react";
import type { TotalRule } from "../../api/client";
import { Button, IconButton, Select, Txt } from "../../ui";

const RULES: { value: TotalRule; label: string }[] = [
  { value: "sum", label: "added up" },
  { value: "first", label: "the first member's value" },
  { value: "last", label: "the last member's value" },
  { value: "none", label: "no total" },
];

// A metric's own total rules over dimensions (metric.dimension_rules,
// migration 174), by dimension id: they win over the dimension's own rule
// (Dimensions › Members total). A time dimension totals by the metric's
// time summary instead.
export function TotalRulesEditor({ rules, onChange, dims }: {
  rules: Record<string, TotalRule>;
  onChange: (rules: Record<string, TotalRule>) => void;
  /** The revision's dimensions that take a rule (not time). */
  dims: { id: string; name: string; agg_rule?: string }[];
}) {
  const entries = Object.entries(rules);
  const free = dims.filter(d => !(d.id in rules));
  const ruleOf = (r?: string) => RULES.find(x => x.value === (r || "sum"))?.label ?? r;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
      {entries.map(([dimId, rule], i) => {
        const dim = dims.find(d => d.id === dimId);
        return (
          <div key={dimId} style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
            <span className="mvx-admin-muted">over</span>
            <Select value={dimId} aria-label={`Total rule ${i + 1} dimension`}
              onChange={e => {
                const next: Record<string, TotalRule> = {};
                for (const [d, r] of entries) next[d === dimId ? e.target.value : d] = r;
                onChange(next);
              }}>
              <option value={dimId}>{dim?.name ?? dimId}</option>
              {free.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
            </Select>
            <span className="mvx-admin-muted">its total is</span>
            <Select value={rule} aria-label={`Total rule ${i + 1}`} onChange={e => onChange({ ...rules, [dimId]: e.target.value as TotalRule })}>
              {RULES.map(r => <option key={r.value} value={r.value}>{r.label}</option>)}
            </Select>
            {dim && <span className="mvx-admin-muted">(the dimension's: <Txt>{ruleOf(dim.agg_rule)}</Txt><Txt>)</Txt></span>}
            <IconButton aria-label={`Remove total rule ${i + 1}`} onClick={() => {
              const next = { ...rules };
              delete next[dimId];
              onChange(next);
            }}><XIcon size={14} /></IconButton>
          </div>
        );
      })}
      {free.length > 0 && (
        <div>
          <Button size="sm" onClick={() => onChange({ ...rules, [free[0].id]: free[0].agg_rule === "first" ? "sum" : "first" })}>
            <Plus size={14} /> Total over a dimension
          </Button>
        </div>
      )}
    </div>
  );
}
