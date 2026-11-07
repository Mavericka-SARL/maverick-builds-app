import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api, type GridData } from "../../api/client";
import { useDemo } from "../../api/useDemo";
import { invalidateModelData } from "../modelDataQueries";
import { Button, Card, EmptyState, Field, LoadingState, Select } from "../../ui";

/**
 * Business Admin › Copy values: the values typed into one member written to
 * another in the live revision — the plan into the budget — each checked as a
 * typed value is, all or nothing (2026-11-07). A lock workflow then freezes
 * the copy, and Reopen takes it back.
 */
export function CopyValuesTab() {
  const qc = useQueryClient();
  const { data: ctx } = useDemo();
  const { data: meta, isLoading } = useQuery({
    queryKey: ["grid-meta", ctx?.revision_id],
    queryFn: () => api.getGrid(ctx!.revision_id, undefined, { metaOnly: true }),
    enabled: !!ctx?.revision_id,
    staleTime: 30_000,
  });
  const dims = useMemo(() => {
    const g = meta as GridData | undefined;
    return (g?.all_dimensions ?? g?.dimensions ?? []).filter(d => d.members.length > 1);
  }, [meta]);
  const [dimId, setDimId] = useState("");
  const [from, setFrom] = useState("");
  const [to, setTo] = useState("");
  const dim = dims.find(d => d.id === dimId);
  const leaves = useMemo(() => {
    const parents = new Set((dim?.members ?? []).map(m => m.parent_code).filter(Boolean));
    return (dim?.members ?? []).filter(m => !parents.has(m.code));
  }, [dim]);
  const body = { dimension_id: dimId, from, to };
  const check = useMutation({ mutationFn: () => api.copyBAValues({ ...body, dry_run: true }) });
  const copy = useMutation({
    mutationFn: () => api.copyBAValues(body),
    onSuccess: () => { void invalidateModelData(qc); },
  });
  const reset = () => { check.reset(); copy.reset(); };

  if (isLoading || !ctx) return <LoadingState label="Loading dimensions…" />;
  if (dims.length === 0) return <EmptyState label="This model has no dimension to copy along." />;
  const ready = !!dimId && !!from && !!to && from !== to;
  const label = (code: string) => leaves.find(m => m.code === code)?.label || code;
  return (
    <div style={{ display: "flex", flexDirection: "column", gap: 12, maxWidth: 640 }}>
      <p className="mvx-admin-muted" style={{ margin: 0, fontSize: 13 }}>
        Write every value typed into one member to the same cells of another, in the live revision: the plan into the
        budget, one scenario into another. Values that records post stay with their records. Locks and access rules
        apply as when typing; if any value cannot be written, nothing is copied. Lock the copy afterwards with the
        model's lock workflow.
      </p>
      <Card padding={14}>
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr 1fr", gap: 10 }}>
          <Field label="Along">
            <Select value={dimId} aria-label="Dimension" onChange={e => { setDimId(e.target.value); setFrom(""); setTo(""); reset(); }}>
              <option value="">— dimension —</option>
              {dims.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
            </Select>
          </Field>
          <Field label="From">
            <Select value={from} aria-label="Copy from" disabled={!dim} onChange={e => { setFrom(e.target.value); reset(); }}>
              <option value="">— member —</option>
              {leaves.map(m => <option key={m.code} value={m.code}>{m.label || m.code}</option>)}
            </Select>
          </Field>
          <Field label="To">
            <Select value={to} aria-label="Copy to" disabled={!dim} onChange={e => { setTo(e.target.value); reset(); }}>
              <option value="">— member —</option>
              {leaves.filter(m => m.code !== from).map(m => <option key={m.code} value={m.code}>{m.label || m.code}</option>)}
            </Select>
          </Field>
        </div>
        <div style={{ display: "flex", gap: 8, marginTop: 12, alignItems: "center" }}>
          <Button variant="secondary" disabled={!ready || check.isPending} loading={check.isPending} onClick={() => check.mutate()}>Check</Button>
          <Button variant="primary" disabled={!ready || !check.isSuccess || check.data.cells === 0 || copy.isPending || copy.isSuccess}
            loading={copy.isPending} onClick={() => copy.mutate()}>
            Copy
          </Button>
          {check.isSuccess && !copy.isSuccess && (
            <span role="status" style={{ fontSize: 13 }}>
              {check.data.cells === 0 ? `${label(from)} holds no typed values.` : `${check.data.cells} values will be written to ${label(to)}.`}
            </span>
          )}
          {copy.isSuccess && <span role="status" style={{ fontSize: 13 }}>Copied {copy.data.cells} values to {label(to)}.</span>}
        </div>
        {(check.error || copy.error) && <p className="mvx-admin-error" style={{ margin: "8px 0 0" }}>{((check.error || copy.error) as Error).message}</p>}
      </Card>
    </div>
  );
}
