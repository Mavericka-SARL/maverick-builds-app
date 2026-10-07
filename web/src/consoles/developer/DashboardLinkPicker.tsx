import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api, type AppInfo, type DashboardDef } from "../../api/client";
import { Button, Select, TextInput } from "../../ui";
import { dashboardLinkHref } from "../business/dashboardLinks";

/**
 * Builds a text widget's link to a dashboard — of this model or of any other
 * the developer can open — so nobody has to type names exactly. It writes the
 * same Markdown a person could type (consoles/business/dashboardLinks):
 * the link carries names, not ids, and there is nothing else to save.
 */
export function DashboardLinkPicker({ modelId, revisionId, onInsert }: {
  modelId: string;
  /** The revision being designed: this model's own dashboards come from it. */
  revisionId?: string;
  onInsert: (markdown: string) => void;
}) {
  const [open, setOpen] = useState(false);
  const [target, setTarget] = useState(modelId);
  const [dashboard, setDashboard] = useState("");
  const [label, setLabel] = useState("");

  const { data: apps = [] } = useQuery({ queryKey: ["apps"], queryFn: api.getApps, enabled: open });
  const models = (apps as AppInfo[]).flatMap(app => (app.models ?? []).map(model => ({ app, model })));
  const here = target === modelId;
  const picked = models.find(m => m.model.id === target);

  const { data: dashboards = [], isLoading } = useQuery({
    queryKey: ["dashboard-link-targets", target, here ? revisionId : "live"],
    queryFn: () => (here ? api.listDashboards(revisionId) : api.listModelDashboards(picked!.app.id, target)),
    enabled: open && (here || !!picked),
  });

  if (!open) {
    return <Button size="sm" variant="ghost" onClick={() => setOpen(true)}>Link to a dashboard…</Button>;
  }

  const modelName = here ? null : picked?.model.name ?? null;
  const text = label.trim() || dashboard || modelName || "";
  const canInsert = !!text && (here ? !!dashboard : !!picked);

  function insert() {
    onInsert(`[${text.replace(/[[\]]/g, "")}](${dashboardLinkHref(modelName, dashboard || null)})`);
    setOpen(false);
    setDashboard("");
    setLabel("");
  }

  return (
    <div style={{ display: "grid", gap: 6, padding: 8, marginTop: 6, border: "1px solid var(--color-border)", borderRadius: 6 }}>
      <Select aria-label="Link to model" value={target} onChange={e => { setTarget(e.target.value); setDashboard(""); }}>
        {!models.some(m => m.model.id === modelId) && <option value={modelId}>This model</option>}
        {models.map(({ app, model }) => (
          <option key={model.id} value={model.id}>{model.id === modelId ? `${model.name} (this model)` : (apps as AppInfo[]).length > 1 ? `${app.name} — ${model.name}` : model.name}</option>
        ))}
      </Select>
      <Select aria-label="Link to dashboard" value={dashboard} onChange={e => setDashboard(e.target.value)} disabled={isLoading}>
        <option value="">{here ? "— pick a dashboard —" : "Its first dashboard"}</option>
        {(dashboards as DashboardDef[]).map(d => <option key={d.id} value={d.name}>{d.name}</option>)}
      </Select>
      <TextInput aria-label="Link text" value={label} onChange={e => setLabel(e.target.value)} placeholder={dashboard || modelName || "Link text"} />
      <div style={{ display: "flex", gap: 6 }}>
        <Button size="sm" onClick={insert} disabled={!canInsert}>Insert link</Button>
        <Button size="sm" variant="ghost" onClick={() => setOpen(false)}>Cancel</Button>
      </div>
    </div>
  );
}

