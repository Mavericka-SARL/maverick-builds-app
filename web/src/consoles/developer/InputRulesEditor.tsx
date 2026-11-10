import { useState } from "react";
import type { InputRules } from "../../api/client";
import { TextInput } from "../../ui";

// The values an input metric's cells accept (metric.input_rules): every
// write of another value is refused — typed, pasted, imported, a
// connector's, the AI Developer's. A blank bound accepts any value.
export function InputRulesEditor({ rules, onChange, unit }: {
  rules: InputRules;
  onChange: (rules: InputRules) => void;
  /** Shown after a bound: "%" for a percentage (stored as 15 for 15%). */
  unit?: string;
}) {
  // The bounds as typed: "-" on the way to "-5" is no number yet.
  const [minText, setMinText] = useState(rules.min === undefined ? "" : String(rules.min));
  const [maxText, setMaxText] = useState(rules.max === undefined ? "" : String(rules.max));
  const bound = (key: "min" | "max", text: string) => {
    (key === "min" ? setMinText : setMaxText)(text);
    const next = { ...rules };
    const n = Number(text.trim());
    if (text.trim() === "" || !Number.isFinite(n)) delete next[key];
    else next[key] = n;
    onChange(next);
  };
  return (
    <div style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
      <span className="mvx-admin-muted">from</span>
      <TextInput inputMode="decimal" value={minText} placeholder="any" style={{ width: 80 }} aria-label="Lowest accepted value"
        onChange={e => bound("min", e.target.value)} />
      {unit && <span className="mvx-admin-muted">{unit}</span>}
      <span className="mvx-admin-muted">to</span>
      <TextInput inputMode="decimal" value={maxText} placeholder="any" style={{ width: 80 }} aria-label="Highest accepted value"
        onChange={e => bound("max", e.target.value)} />
      {unit && <span className="mvx-admin-muted">{unit}</span>}
      <label style={{ display: "flex", alignItems: "center", gap: 4, fontSize: 12, marginLeft: 8 }}>
        <input type="checkbox" aria-label="Whole numbers only" checked={!!rules.whole}
          onChange={e => {
            const next = { ...rules };
            if (e.target.checked) next.whole = true;
            else delete next.whole;
            onChange(next);
          }} />
        Whole numbers only
      </label>
    </div>
  );
}
