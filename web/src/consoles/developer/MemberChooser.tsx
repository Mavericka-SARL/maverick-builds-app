import { Checkbox } from "../../ui";

/**
 * Which members of a dimension a selector offers or a chart plots, by code,
 * in the dimension's order: none ticked means all of them (2026-11-07; a
 * Data view selector offering seven misleading views, a scenario chart that
 * must show only the two "vs Base" bars).
 */
export function MemberChooser({ label, members, value, onChange }: {
  label: string;
  members: { code: string; label?: string }[];
  value: string[];
  onChange: (codes: string[]) => void;
}) {
  const chosen = new Set(value);
  return (
    <details className="mvx-member-chooser" style={{ fontSize: 12 }}>
      <summary aria-label={label} style={{ cursor: "pointer" }}>
        {value.length === 0 ? "all members" : `${value.length} of ${members.length} members`}
      </summary>
      <div style={{ display: "flex", flexDirection: "column", gap: 2, maxHeight: 200, overflowY: "auto", padding: "6px 0" }}>
        {members.map(m => (
          <Checkbox key={m.code} checked={chosen.has(m.code)} label={m.label || m.code}
            onChange={e => {
              const next = new Set(chosen);
              if (e.target.checked) next.add(m.code); else next.delete(m.code);
              onChange(members.map(x => x.code).filter(c => next.has(c)));
            }} />
        ))}
      </div>
    </details>
  );
}
