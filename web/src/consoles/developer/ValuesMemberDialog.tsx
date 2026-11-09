import { useState } from "react";
import type { DevDimension } from "../../api/client";
import { Button, Dialog, Field, Select, Txt } from "../../ui";

/** What a grid change needs to know before it can move the values already stored. */
export interface ValuesMemberNeed {
  /** What is being saved: "adding Scenario", "moving Revenue onto this grid". */
  what: string;
  /** The dimensions the values have no member of. */
  dimension_ids: string[];
  /** How many stored values that is. */
  count: number;
}

/**
 * Asks which member of each dimension the values already stored belong to,
 * when a grid gains a dimension or a metric moves onto a grid with one its
 * values lack (409 VALUES_NEED_MEMBER). Only members that take input are
 * offered: the leaves of the dimension.
 */
export function ValuesMemberDialog({ need, dims, onDone }: {
  need: ValuesMemberNeed; dims: DevDimension[]; onDone: (members: Record<string, string> | null) => void;
}) {
  const asked = need.dimension_ids.map(id => dims.find(d => d.id === id)).filter((d): d is DevDimension => !!d);
  const leaves = (d: DevDimension) => d.members.filter(m => !d.members.some(c => c.parent_member_id === m.id));
  const [chosen, setChosen] = useState<Record<string, string>>(() =>
    Object.fromEntries(asked.map(d => [d.id, leaves(d)[0]?.code ?? ""])));
  const ready = asked.length === need.dimension_ids.length && asked.every(d => chosen[d.id]);
  return (
    <Dialog open onClose={() => onDone(null)} title="Which member do the values belong to?">
      <div style={{ display: "flex", flexDirection: "column", gap: 12 }}>
        <p style={{ margin: 0 }}>
          <Txt>{`${need.count} value${need.count === 1 ? " is" : "s are"} already stored without ${asked.map(d => d.label || d.name).join(", ")}. Before ${need.what}, choose the member they belong to; they are filed there.`}</Txt>
        </p>
        {asked.map(d => (
          <Field key={d.id} label={d.label || d.name}>
            <Select aria-label={`Member of ${d.label || d.name}`} value={chosen[d.id] ?? ""}
              onChange={e => setChosen(c => ({ ...c, [d.id]: e.target.value }))}>
              {leaves(d).length === 0 && <option value="">No member yet: add one first</option>}
              {leaves(d).map(m => <option key={m.id} value={m.code}>{m.label || m.code}</option>)}
            </Select>
          </Field>
        ))}
        <div style={{ display: "flex", gap: 8, justifyContent: "flex-end" }}>
          <Button variant="secondary" onClick={() => onDone(null)}>Cancel</Button>
          <Button variant="primary" disabled={!ready} onClick={() => onDone(chosen)}>File the values there</Button>
        </div>
      </div>
    </Dialog>
  );
}
