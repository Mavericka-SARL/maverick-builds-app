import { useBrand } from "../../../branding/brand";
import { useQuery } from "@tanstack/react-query";
import { api, type ApiIntegrationConfig, type FormDef } from "../../../api/client";
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
          onChange={e => {
            const direction = e.target.value as ApiIntegrationConfig["direction"];
            // SFTP files and other models are read: a push goes back to an
            // HTTPS call.
            onConfig(direction === "push" && (config.protocol === "sftp" || config.protocol === "model")
              ? { direction, protocol: undefined, sftp: undefined, model: undefined, auth: { type: "none" } }
              : { direction });
          }}>
          <option value="pull">Pull — external API → {brand.name}</option>
          <option value="push">Push — {brand.name} → external API</option>
        </Select>
      </Field>
      <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12 }}>
        <Field label="Target type">
          <Select value={config.target_type} aria-label="Target type"
            onChange={e => onConfig({ target_type: e.target.value as ApiIntegrationConfig["target_type"], target_id: "", replace_by: undefined })}>
            <option value="grid">Grid</option>
            <option value="form">Table of records</option>
            <option value="dimension">Dimension</option>
          </Select>
        </Field>
        <Field label={config.direction === "pull" ? "Import into" : "Read from"} required>
          <Select value={config.target_id} aria-label="Target" onChange={e => onConfig({ target_id: e.target.value, replace_by: undefined })}>
            <option value="">Select…</option>
            {targets.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}
          </Select>
        </Field>
      </div>
      {/* A grid's mode. Full reload clears the target grid — every metric
          of it — not the revision: it used to clear every grid
          (2026-11-09), while this said "the target". */}
      {/* A table's loads have no import mode: "When a run brings rows
          again" below is the table's own (owner decision 2026-11-09 — a
          pull into a table ignored the mode it showed). */}
      {config.direction === "pull" && config.target_type !== "form" && (
        <Field label="Import mode" description={config.protocol === "model"
          ? "A model link reads the source's whole values at each run: replace listed cells overwrites the cells a run brings; full reload first deletes every value of the target grid (other grids keep theirs)"
          : "incremental adds to existing values; replace listed cells overwrites the cells a run brings; full reload first deletes every value of the target grid (other grids keep theirs)"}>
          <Select value={config.import_mode ?? "incremental"} aria-label="Import mode"
            onChange={e => onConfig({ import_mode: e.target.value as ApiIntegrationConfig["import_mode"] })}>
            <option value="incremental" disabled={config.protocol === "model"}>Incremental</option>
            <option value="replace">Replace listed cells</option>
            <option value="full_reload">Full reload</option>
          </Select>
        </Field>
      )}
      {config.direction === "pull" && config.target_type === "form" && config.target_id && (
        <Field label="When a run brings rows again" description={config.protocol === "model"
          ? "A model link reads the source's whole rows at each run: choose the field whose records each run replaces, or every run adds them again."
          : "Rows of a period (or site, ledger…) a run already loaded are otherwise added again."}>
          <Select value={config.replace_by ?? ""} aria-label="Replace by"
            onChange={e => onConfig({ replace_by: e.target.value || undefined })}>
            {/* A model link brings the source's whole rows each run: adding
                them would double the table (2026-11-09). A table that keeps
                a dimension updates its records by their code instead. */}
            <option value="" disabled={config.protocol === "model" && !(forms as FormDef[]).find(f => f.id === config.target_id)?.keeps_dimension_id}>
              {(forms as FormDef[]).find(f => f.id === config.target_id)?.keeps_dimension_id ? "Update the records by their code" : "Add them to the earlier records"}
            </option>
            {(forms as FormDef[]).find(f => f.id === config.target_id)?.keeps_dimension_id ? null
              : ((forms as FormDef[]).find(f => f.id === config.target_id)?.fields ?? [])
                .filter(f => f.type === "dimension" || f.type === "select" || f.type === "text" || f.type === "date")
                .map(f => <option key={f.name} value={f.name}>{`Replace the records with the same ${f.label || f.name}`}</option>)}
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
