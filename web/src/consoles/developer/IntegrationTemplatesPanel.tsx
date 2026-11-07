import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../../api/client";
import { Button, Card, Select, TextInput, useConfirm, Txt } from "../../ui";

/**
 * Integration templates (2026-11-07): a loader or an export of this model
 * saved for every model of the tenant, and made again here with its target
 * grid or table found by name. A REST connector or a model link belongs to
 * its model.
 */
export function IntegrationTemplatesPanel({ revisionId }: { revisionId?: string }) {
  const qc = useQueryClient();
  const { confirm, confirmElement } = useConfirm();
  const { data: templates = [] } = useQuery({ queryKey: ["integration-templates"], queryFn: api.listIntegrationTemplates });
  const { data: integrations = [] } = useQuery({
    queryKey: ["integrations", revisionId],
    queryFn: () => api.listIntegrations(revisionId),
  });
  const savable = integrations.filter(i => i.type !== "rest_api" && i.type !== "model");
  const [source, setSource] = useState("");
  const [picked, setPicked] = useState("");
  const [name, setName] = useState("");
  const save = useMutation({
    mutationFn: () => api.saveIntegrationTemplate(source),
    onSuccess: () => { setSource(""); qc.invalidateQueries({ queryKey: ["integration-templates"] }); },
  });
  const use = useMutation({
    mutationFn: () => api.useIntegrationTemplate(picked, name.trim(), revisionId),
    onSuccess: () => { setName(""); qc.invalidateQueries({ queryKey: ["integrations"] }); },
  });
  const tpl = templates.find(t => t.id === picked);
  const error = (save.error || use.error) as Error | null;
  return (
    <Card padding={14} style={{ marginTop: 20 }}>
      <div style={{ fontWeight: 600, marginBottom: 4 }}>Templates</div>
      <p className="mvx-admin-muted" style={{ margin: "0 0 10px", fontSize: 13 }}>
        Save a loader or an export to reuse it in any model of this tenant. Made in a model, it loads into the grid or
        table of the same name there.
      </p>
      <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap" }}>
        <Select value={source} aria-label="Integration to save as a template" onChange={e => setSource(e.target.value)} style={{ minWidth: 240 }}>
          <option value="">— an integration of this model —</option>
          {savable.map(i => <option key={i.id} value={i.id}>{i.name}</option>)}
        </Select>
        <Button size="sm" disabled={!source || save.isPending} loading={save.isPending} onClick={() => save.mutate()}>Save as template</Button>
      </div>
      {templates.length > 0 && (
        <div style={{ display: "flex", gap: 8, alignItems: "center", flexWrap: "wrap", marginTop: 10 }}>
          <Select value={picked} aria-label="Template" onChange={e => { setPicked(e.target.value); setName(templates.find(t => t.id === e.target.value)?.name ?? ""); }} style={{ minWidth: 240 }}>
            <option value="">— a template —</option>
            {templates.map(t => <option key={t.id} value={t.id}>{t.name}<Txt>{t.target_name ? ` → ${t.target_name}` : ""}</Txt></option>)}
          </Select>
          <TextInput value={name} aria-label="Name in this model" placeholder="Name in this model" onChange={e => setName(e.target.value)} style={{ width: 220 }} />
          <Button size="sm" variant="primary" disabled={!picked || use.isPending} loading={use.isPending} onClick={() => use.mutate()}>Make here</Button>
          <Button size="sm" variant="ghost" disabled={!tpl}
            onClick={() => tpl && confirm({
              title: "Delete template?", body: `"${tpl.name}" is removed for every model; integrations made from it stay.`,
              confirmLabel: "Delete template",
              onConfirm: async () => { await api.deleteIntegrationTemplate(tpl.id); setPicked(""); qc.invalidateQueries({ queryKey: ["integration-templates"] }); },
            })}>
            Delete
          </Button>
        </div>
      )}
      {use.isSuccess && <p role="status" style={{ fontSize: 13, margin: "8px 0 0" }}>Made. It is listed with this model's integrations.</p>}
      {error && <p className="mvx-admin-error" style={{ margin: "8px 0 0" }}>{error.message}</p>}
      {confirmElement}
    </Card>
  );
}
