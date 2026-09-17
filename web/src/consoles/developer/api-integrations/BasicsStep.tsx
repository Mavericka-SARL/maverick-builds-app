import { useBrand } from "../../../branding/brand";
import { useQuery } from "@tanstack/react-query";
import { api, type ApiIntegrationConfig } from "../../../api/client";
import { Field, Select, TextInput, Textarea } from "../../../ui";

export function BasicsStep({
  name, setName, description, setDescription, tags, setTags,
  config, onConfig, revisionId,
}: {
  name: string; setName: (v: string) => void;
  description: string; setDescription: (v: string) => void;
  tags: string; setTags: (v: string) => void;
  config: ApiIntegrationConfig;
  onConfig: (patch: Partial<ApiIntegrationConfig>) => void;
  revisionId?: string;
}) {
  const brand = useBrand();
  const { data: grids = [] } = useQuery({ queryKey: ["dev-grids", revisionId], queryFn: () => api.listGrids(revisionId) });
  const { data: forms = [] } = useQuery({ queryKey: ["dev-forms", revisionId], queryFn: () => api.listForms(revisionId) });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions-all", revisionId], queryFn: () => api.getDevDimensions(revisionId) });

  const targets: { id: string; name: string }[] =
    config.target_type === "grid" ? (grids as { id: string; name: string }[])
    : config.target_type === "form" ? (forms as { id: string; name: string }[])
    : (dims as { id: string; name: string }[]);

  return (
    <div style={{ display: "grid", gap: 12, maxWidth: 560 }}>
      <Field label="Name" required>
        <TextInput value={name} onChange={e => setName(e.target.value)} placeholder="Billing API sync" aria-label="Integration name" />
      </Field>
      <Field label="Description">
        <Textarea value={description} onChange={e => setDescription(e.target.value)} rows={2} aria-label="Description" />
      </Field>
      <Field label="Tags" description="Comma-separated">
        <TextInput value={tags} onChange={e => setTags(e.target.value)} placeholder="billing, nightly" aria-label="Tags" />
      </Field>
      <Field label="Direction">
        <Select value={config.direction} aria-label="Direction"
          onChange={e => onConfig({ direction: e.target.value as ApiIntegrationConfig["direction"] })}>
          <option value="pull">Pull — external API → {brand.name}</option>
          <option value="push">Push — {brand.name} → external API</option>
        </Select>
      </Field>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
        <Field label="Target type">
          <Select value={config.target_type} aria-label="Target type"
            onChange={e => onConfig({ target_type: e.target.value as ApiIntegrationConfig["target_type"], target_id: "" })}>
            <option value="grid">Grid</option>
            <option value="form">Form</option>
            <option value="dimension">Dimension</option>
          </Select>
        </Field>
        <Field label={config.direction === "pull" ? "Import into" : "Read from"} required>
          <Select value={config.target_id} aria-label="Target" onChange={e => onConfig({ target_id: e.target.value })}>
            <option value="">Select…</option>
            {targets.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
          </Select>
        </Field>
      </div>
      {config.direction === "pull" && (
        <Field label="Import mode" description="incremental adds to existing values; replace overwrites matched cells; full reload wipes the target first">
          <Select value={config.import_mode ?? "incremental"} aria-label="Import mode"
            onChange={e => onConfig({ import_mode: e.target.value as ApiIntegrationConfig["import_mode"] })}>
            <option value="incremental">Incremental</option>
            <option value="replace">Replace</option>
            <option value="full_reload">Full reload</option>
          </Select>
        </Field>
      )}
      {config.direction === "push" && (
        <Field label="Push mode">
          <Select value={config.mapping.batch ? "batch" : "per_record"} aria-label="Push mode"
            onChange={e => onConfig({ mapping: { ...config.mapping, batch: e.target.value === "batch" } })}>
            <option value="per_record">One request per record</option>
            <option value="batch">One batched JSON request</option>
          </Select>
        </Field>
      )}
      <Field label="Revision" description="This integration reads/writes this revision">
        <TextInput value={revisionId ?? "(active revision)"} disabled aria-label="Revision" />
      </Field>
    </div>
  );
}
