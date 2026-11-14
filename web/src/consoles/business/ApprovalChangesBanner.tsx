import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type ApprovalChangeRun } from "../../api/client";
import { Button, InlineAlert, Txt } from "../../ui";
import { useDateFormat } from "../dateFormat";
import { momentDate } from "../dateSerial";

const num = (v: number | undefined, text?: string) =>
  text ? text : v === undefined || v === null ? "—" : v.toLocaleString(undefined, { maximumFractionDigits: 4 });

/**
 * Says, above a dashboard, which approved figures changed since their approval
 * (owner decision 2026-11-13): a released period stays locked, but an input
 * outside the lock — a tax rate, a posting account, a setting — can still move
 * its figures. Nothing is shown when no approved figure moved.
 */
export function ApprovalChangesBanner({ modelId }: { modelId: string }) {
  const dateFormat = useDateFormat();
  const [open, setOpen] = useState<string | null>(null);
  const { data } = useQuery({
    queryKey: ["approval-changes", modelId],
    queryFn: api.approvalChanges,
    refetchInterval: 60_000,
  });
  const runs: ApprovalChangeRun[] = data?.runs ?? [];
  if (runs.length === 0) return null;
  return (
    <div style={{ display: "grid", gap: 8, marginBottom: 16 }} aria-label="Approved figures changed since approval">
      {runs.map(r => (
        <InlineAlert key={r.instance_id} tone="warning">
          <div style={{ display: "flex", gap: 12, alignItems: "baseline", flexWrap: "wrap" }}>
            <span>
              <strong>Changed since approval:</strong> {r.workflow}<Txt>{r.scope ? ` (${r.scope})` : ""}</Txt><Txt>, approved </Txt>{momentDate(r.approved_at, dateFormat)} —{" "}
              {r.changed} figure<Txt>{r.changed === 1 ? "" : "s"}</Txt> <Txt>no longer what was approved. Something outside the lock moved </Txt>{r.changed === 1 ? "it" : "them"} — an input without the period (a rate, a setting) or a change to the model; reopen the run to change the period on purpose, or undo that change.
            </span>
            {r.examples.length > 0 && (
              <Button size="sm" variant="ghost" onClick={() => setOpen(open === r.instance_id ? null : r.instance_id)}>
                {open === r.instance_id ? "Hide" : "Show"} examples
              </Button>
            )}
          </div>
          {open === r.instance_id && (
            <ul style={{ margin: "8px 0 0", paddingLeft: 18 }}>
              {r.examples.map((e, i) => (
                <li key={i}>
                  {e.metric}
                  <Txt>{Object.keys(e.members).length > 0 && ` · ${Object.entries(e.members).map(([d, m]) => `${d}: ${m}`).join(", ")}`}</Txt>
                  <Txt>{": "}</Txt>approved {num(e.approved, e.approved_text)}, now {num(e.now, e.now_text)}
                </li>
              ))}
            </ul>
          )}
        </InlineAlert>
      ))}
    </div>
  );
}
