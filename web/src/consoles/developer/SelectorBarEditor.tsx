import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { X } from "lucide-react";
import { api, type DevDimension, type SelectorBarEntry } from "../../api/client";
import { defaultLeafCode } from "../dashboardLayout";
import { Button, IconButton, Select } from "../../ui";

// The dashboard's selector bar, as the developer sets it: the dimensions
// whose selector is drawn once at the top of the dashboard, each with the
// member it opens on. Saved on its own (PATCH selector_bar), not with the
// layout.
export function SelectorBarEditor({ dashId, bar, dims, onSaved, onClose }: {
  dashId: string;
  bar: SelectorBarEntry[];
  dims: DevDimension[];
  onSaved: () => void;
  onClose: () => void;
}) {
  const [draft, setDraft] = useState<SelectorBarEntry[]>(bar);
  const save = useMutation({
    mutationFn: () => api.updateDashboard(dashId, { selector_bar: draft }),
    onSuccess: onSaved,
  });
  const unused = dims.filter(d => !draft.some(e => e.dimension_id === d.id));
  const changed = JSON.stringify(draft) !== JSON.stringify(bar);

  const firstCode = (d: DevDimension | undefined) =>
    d ? defaultLeafCode({ members: d.members.map(m => ({ code: m.code, parent_code: d.members.find(p => p.id === m.parent_member_id)?.code })) }) ?? "" : "";

  return (
    <div className="mvx-panel" style={{ padding: 20, marginBottom: 16, background: "var(--color-surface-subtle)" }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8, marginBottom: 6 }}>
        <span style={{ fontSize: 13, fontWeight: 600, flex: 1 }}>Selector bar</span>
        <IconButton aria-label="Close" title="Close" size={26} onClick={onClose}>
          <X size={14} />
        </IconButton>
      </div>
      <p className="mvx-admin-muted" style={{ fontSize: 12, margin: "0 0 12px" }}>
        One selector per dimension at the top of the dashboard. Every grid and chart follows it and draws no
        selector of its own for these dimensions; so do KPIs set to “Follow dashboard selectors” or “Follow the
        selector bar”.
      </p>
      {draft.length === 0 && <p className="mvx-admin-muted" style={{ fontSize: 13 }}>No selector bar: each widget draws its own selectors.</p>}
      {draft.map((e, i) => {
        const dim = dims.find(d => d.id === e.dimension_id);
        return (
          <div key={e.dimension_id} style={{ display: "flex", gap: 8, alignItems: "center", marginBottom: 8 }}>
            <Select
              aria-label={`Selector ${i + 1} dimension`}
              value={e.dimension_id}
              onChange={ev => {
                const next = dims.find(d => d.id === ev.target.value);
                setDraft(prev => prev.map((p, j) => (j === i ? { dimension_id: ev.target.value, default: firstCode(next) } : p)));
              }}
              style={{ minWidth: 180 }}
            >
              {dim ? <option value={dim.id}>{dim.name}</option> : <option value={e.dimension_id}>(missing dimension)</option>}
              {unused.map(d => <option key={d.id} value={d.id}>{d.name}</option>)}
            </Select>
            <span className="mvx-admin-muted" style={{ fontSize: 12 }}>opens on</span>
            <Select
              aria-label={`Selector ${i + 1} default member`}
              value={e.default ?? ""}
              onChange={ev => setDraft(prev => prev.map((p, j) => (j === i ? { ...p, default: ev.target.value } : p)))}
              style={{ minWidth: 180 }}
            >
              <option value="">(first member)</option>
              {(dim?.members ?? []).map(m => <option key={m.code} value={m.code}>{m.label || m.code}</option>)}
            </Select>
            <IconButton aria-label={`Remove selector ${dim?.name ?? i + 1}`} title="Remove" size={26}
              onClick={() => setDraft(prev => prev.filter((_, j) => j !== i))}>
              <X size={14} />
            </IconButton>
          </div>
        );
      })}
      <div style={{ display: "flex", gap: 8, marginTop: 12 }}>
        <Button
          size="sm"
          variant="secondary"
          disabled={unused.length === 0 || draft.length >= 8}
          onClick={() => setDraft(prev => [...prev, { dimension_id: unused[0].id, default: firstCode(unused[0]) }])}
        >
          + Add dimension
        </Button>
        <Button size="sm" variant="primary" disabled={!changed} loading={save.isPending} loadingLabel="Saving…" onClick={() => save.mutate()}>
          Save selector bar
        </Button>
      </div>
      {save.isError && <p className="mvx-admin-error" style={{ marginTop: 8 }}>{(save.error as Error).message}</p>}
    </div>
  );
}
