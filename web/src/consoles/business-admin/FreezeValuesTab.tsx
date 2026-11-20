import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, dimLabel, type GridData } from "../../api/client";
import { useDemo } from "../../api/useDemo";
import { invalidateModelData } from "../modelDataQueries";
import { Button, Card, EmptyState, Field, LoadingState, Select } from "../../ui";

type Pair = { from: string; to: string };
const NUMERIC = new Set(["", "number", "currency", "percentage", "date"]);

/**
 * Business Admin › Freeze values: metrics' values as they are now —
 * calculated or typed — written into input metrics, so a later formula can
 * compare with them: the week's cash forecast kept for its review once the
 * week closes (2026-11-20). Each value is read as the grid shows it to you
 * and written as a typed value, all or nothing. A workflow approval step
 * can freeze the same way (on_approve.freeze).
 */
export function FreezeValuesTab() {
  const qc = useQueryClient();
  const { data: ctx } = useDemo();
  const { data: meta, isLoading } = useQuery({
    queryKey: ["grid-meta", ctx?.revision_id],
    queryFn: () => api.getGrid(ctx!.revision_id, undefined, { metaOnly: true }),
    enabled: !!ctx?.revision_id,
    staleTime: 30_000,
  });
  const g = meta as GridData | undefined;
  const metrics = useMemo(() => (g?.all_metrics ?? g?.metrics ?? []).filter(m => NUMERIC.has(m.format ?? "")), [g]);
  const inputs = useMemo(() => metrics.filter(m => m.is_input), [metrics]);
  const dimsById = useMemo(() => new Map((g?.all_dimensions ?? g?.dimensions ?? []).map(d => [d.id, d])), [g]);
  const [pairs, setPairs] = useState<Pair[]>([{ from: "", to: "" }]);
  const [at, setAt] = useState<Record<string, string>>({});
  const byId = (id: string) => metrics.find(m => m.id === id);
  // The dimensions the chosen metrics have: each is pinned here, or (shared
  // by both) frozen member by member, or (the source's only) read at its total.
  const dimIds = useMemo(() => {
    const ids = new Set<string>();
    for (const p of pairs) for (const id of [...(byId(p.from)?.dimension_ids ?? []), ...(byId(p.to)?.dimension_ids ?? [])]) ids.add(id);
    return [...ids].filter(id => dimsById.has(id));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pairs, metrics, dimsById]);
  const body = {
    pairs: pairs.filter(p => p.from && p.to),
    at: Object.fromEntries(Object.entries(at).filter(([id, code]) => code && dimIds.includes(id))),
  };
  const check = useMutation({ mutationFn: () => api.freezeBAValues({ ...body, dry_run: true }) });
  const freeze = useMutation({
    mutationFn: () => api.freezeBAValues(body),
    onSuccess: () => { void invalidateModelData(qc); },
  });
  const reset = () => { check.reset(); freeze.reset(); };
  const setPair = (i: number, p: Partial<Pair>) => { setPairs(ps => ps.map((x, j) => (j === i ? { ...x, ...p } : x))); reset(); };

  if (isLoading || !ctx) return <LoadingState label="Loading metrics…" />;
  if (inputs.length === 0) return <EmptyState label="This model has no input metric to freeze values into." />;
  const ready = body.pairs.length > 0;
  const shown = (check.data?.values ?? []).slice(0, 30);
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12, maxWidth: 760 }}>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 13 }}>
        Keep metrics' values as they are now — calculated or typed — in input metrics, so a formula can compare with them
        later: the coming week's forecast, kept for the review once the week closes. Each value is read as the grid shows
        it to you and written as a typed value; locks and access rules apply, and if any value cannot be written, nothing
        is frozen.
      </p>
      <Card padding={14}>
        {pairs.map((p, i) => (
          <div key={i} style={{ display: "grid", gridTemplateColumns: "1fr 1fr auto", gap: 10, alignItems: "end", marginBottom: 8 }}>
            <Field label={i === 0 ? "Freeze the values of" : ""}>
              <Select value={p.from} aria-label={`Freeze from ${i + 1}`} onChange={e => setPair(i, { from: e.target.value })}>
                <option value="">— metric —</option>
                {metrics.map(m => <option key={m.id} value={m.id}>{m.label || m.name}</option>)}
              </Select>
            </Field>
            <Field label={i === 0 ? "Into the input" : ""}>
              <Select value={p.to} aria-label={`Freeze into ${i + 1}`} onChange={e => setPair(i, { to: e.target.value })}>
                <option value="">— input metric —</option>
                {inputs.filter(m => m.id !== p.from).map(m => <option key={m.id} value={m.id}>{m.label || m.name}</option>)}
              </Select>
            </Field>
            <Button variant="ghost" aria-label={`Remove pair ${i + 1}`} disabled={pairs.length === 1}
              onClick={() => { setPairs(ps => ps.filter((_, j) => j !== i)); reset(); }}>Remove</Button>
          </div>
        ))}
        <Button variant="secondary" onClick={() => { setPairs(ps => [...ps, { from: "", to: "" }]); reset(); }}>Add a pair</Button>
        {dimIds.length > 0 && (
          <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 10, marginTop: 12 }}>
            {dimIds.map(id => {
              const d = dimsById.get(id)!;
              return (
                <Field key={id} label={`At ${dimLabel(d)}`}>
                  <Select value={at[id] ?? ""} aria-label={`At ${dimLabel(d)}`} onChange={e => { setAt(a => ({ ...a, [id]: e.target.value })); reset(); }}>
                    <option value="">— every member (or its total) —</option>
                    {d.members.filter(m => !m.formula).map(m => <option key={m.code} value={m.code}>{m.label || m.code}</option>)}
                  </Select>
                </Field>
              );
            })}
          </div>
        )}
        <div style={{ display: "flex", gap: 8, marginTop: 12, alignItems: "center" }}>
          <Button variant="secondary" disabled={!ready || check.isPending} loading={check.isPending} onClick={() => check.mutate()}>Check</Button>
          <Button variant="primary" disabled={!ready || !check.isSuccess || check.data.cells === 0 || freeze.isPending || freeze.isSuccess}
            loading={freeze.isPending} onClick={() => freeze.mutate()}>
            Freeze
          </Button>
          {check.isSuccess && !freeze.isSuccess && (
            <span role="status" style={{ fontSize: 13 }}>
              {check.data.cells === 0 ? "There is no value to freeze there." : `${check.data.cells} values will be written.`}
            </span>
          )}
          {freeze.isSuccess && <span role="status" style={{ fontSize: 13 }}>Froze {freeze.data.cells} values.</span>}
        </div>
        {shown.length > 0 && !freeze.isSuccess && (
          <table className="mvx-admin-table" style={{ marginTop: 10, fontSize: 12 }}>
            <thead><tr><th>Into</th><th>At</th><th style={{ textAlign: "right" }}>Value</th></tr></thead>
            <tbody>
              {shown.map((v, i) => (
                <tr key={i}>
                  <td>{inputs.find(m => m.name === v.to)?.label || v.to}</td>
                  <td>{Object.entries(v.members).map(([k, c]) => `${k} ${c}`).join(" · ")}</td>
                  <td style={{ textAlign: "right" }}>{v.value.toLocaleString(undefined, { maximumFractionDigits: Math.abs(v.value) >= 1000 ? 0 : 2 })}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
        {(check.error || freeze.error) && <p className="mvx-admin-error" style={{ margin: "8px 0 0" }}>{((check.error || freeze.error) as Error).message}</p>}
      </Card>
    </div>
  );
}
