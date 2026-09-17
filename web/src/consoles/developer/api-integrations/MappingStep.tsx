import { useBrand } from "../../../branding/brand";
import { useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { Plus, Trash2 } from "lucide-react";
import { api, type ApiFieldMap, type ApiIntegrationConfig, type ApiRun } from "../../../api/client";
import { Button, Field, IconButton, InlineAlert, Select, StatusBadge, TextInput } from "../../../ui";
import type { InferredField } from "./integrationMapping";
import { TRANSFORM_KINDS } from "./apiIntegrationTypes";

// Pull: response paths → grid/form/dimension fields (input metrics only for
// values; enough dims+metrics to identify a unique cell — the server's
// ResolveRows enforces it; this step surfaces the requirement up front).
// Push: Mavericks fields → outbound JSON properties.
export function MappingStep({
  config, onConfig, revisionId, inferred, integrationId, saveDraft,
}: {
  config: ApiIntegrationConfig;
  onConfig: (patch: Partial<ApiIntegrationConfig>) => void;
  revisionId?: string;
  inferred: InferredField[];
  integrationId: string | null;
  saveDraft: () => Promise<string | null>;
}) {
  const brand = useBrand();
  const isPull = config.direction === "pull";
  const { data: metrics = [] } = useQuery({ queryKey: ["dev-metrics", revisionId], queryFn: () => api.getMetrics(revisionId) });
  const { data: dims = [] } = useQuery({ queryKey: ["dev-dimensions-all", revisionId], queryFn: () => api.getDevDimensions(revisionId) });

  const inputMetrics = (metrics as { id: string; name: string; is_input?: boolean }[]).filter(m => m.is_input);
  const dimNames = (dims as { id: string; name: string }[]).map(d => d.name);

  const targetOptions: { value: string; label: string; group: string }[] = [];
  if (config.target_type === "grid") {
    for (const d of dimNames) targetOptions.push({ value: d, label: d, group: "Dimensions" });
    for (const m of inputMetrics) targetOptions.push({ value: m.name, label: m.name, group: "Input metrics" });
  } else if (config.target_type === "dimension") {
    for (const v of ["code", "label", "parent_code"]) targetOptions.push({ value: v, label: v, group: "Member" });
  }

  const fields = config.mapping.fields;
  const patchMapping = (p: Partial<ApiIntegrationConfig["mapping"]>) => onConfig({ mapping: { ...config.mapping, ...p } });
  const updField = (i: number, patch: Partial<ApiFieldMap>) => {
    const next = fields.slice();
    next[i] = { ...next[i], ...patch };
    patchMapping({ fields: next });
  };

  const [dryRun, setDryRun] = useState<ApiRun | null>(null);
  const [dryBusy, setDryBusy] = useState(false);
  const runDry = async () => {
    setDryBusy(true);
    setDryRun(null);
    try {
      const id = integrationId ?? await saveDraft();
      if (!id) return;
      const { run_id } = await api.testApiIntegration(id, { dryRun: true, acknowledgeSideEffects: true });
      const poll = window.setInterval(async () => {
        const r = await api.getApiIntegrationRun(run_id);
        if (r.status !== "queued" && r.status !== "running") {
          window.clearInterval(poll);
          setDryRun(r);
          setDryBusy(false);
        }
      }, 1000);
    } catch {
      setDryBusy(false);
    }
  };

  return (
    <div style={{ display: "grid", gap: 12 }}>
      {isPull && config.target_type === "grid" && (
        <Field label="Response shape">
          <Select value={config.mapping.shape ?? "wide"} aria-label="Response shape"
            onChange={e => patchMapping({ shape: e.target.value as "wide" | "long" })} style={{ maxWidth: 260 }}>
            <option value="wide">Wide — one column per metric</option>
            <option value="long">Long — metric name + value fields</option>
          </Select>
        </Field>
      )}
      {isPull && config.mapping.shape === "long" && (
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, maxWidth: 560 }}>
          <Field label="Metric name field" required>
            <SourceInput value={config.mapping.metric_name_source ?? ""} inferred={inferred}
              onChange={v => patchMapping({ metric_name_source: v })} label="Metric name source" />
          </Field>
          <Field label="Value field" required>
            <SourceInput value={config.mapping.value_source ?? ""} inferred={inferred}
              onChange={v => patchMapping({ value_source: v })} label="Value source" />
          </Field>
        </div>
      )}

      <div role="group" aria-label="Field mappings" style={{ display: "grid", gap: 6 }}>
        {fields.map((f, i) => (
          <div key={i} style={{ display: "flex", gap: 6, alignItems: "center", flexWrap: "wrap" }}>
            {isPull ? (
              <SourceInput value={f.source} inferred={inferred} onChange={v => updField(i, { source: v })} label={`Mapping ${i + 1} source`} />
            ) : (
              <TextInput value={f.source} onChange={e => updField(i, { source: e.target.value })}
                placeholder={`${brand.name} field (metric/dim/field name)`} aria-label={`Mapping ${i + 1} source`} style={{ flex: 1, minWidth: 160 }} />
            )}
            <span aria-hidden="true">→</span>
            {isPull && targetOptions.length > 0 ? (
              <Select value={f.target} aria-label={`Mapping ${i + 1} target`}
                onChange={e => updField(i, { target: e.target.value })} style={{ flex: 1, minWidth: 160 }}>
                <option value="">Target…</option>
                {targetOptions.map(o => <option key={o.value} value={o.value}>{o.group}: {o.label}</option>)}
              </Select>
            ) : (
              <TextInput value={f.target} onChange={e => updField(i, { target: e.target.value })}
                placeholder={isPull ? "target field" : "outbound JSON property"} aria-label={`Mapping ${i + 1} target`}
                style={{ flex: 1, minWidth: 160 }} />
            )}
            <TransformEditor field={f} onChange={patch => updField(i, patch)} index={i} />
            <IconButton aria-label={`Remove mapping ${i + 1}`} onClick={() => patchMapping({ fields: fields.filter((_, x) => x !== i) })}>
              <Trash2 size={14} />
            </IconButton>
          </div>
        ))}
        <Button variant="secondary" size="sm" icon={<Plus size={14} />}
          onClick={() => patchMapping({ fields: [...fields, { source: "", target: "" }] })}>
          Add mapping
        </Button>
      </div>

      {!isPull && config.mapping.batch && (
        <div style={{ display: "grid", gridTemplateColumns: "1fr 1fr", gap: 12, maxWidth: 480 }}>
          <Field label="Batch property" description='wrapper array property (default "items")'>
            <TextInput value={config.mapping.batch_property ?? ""} onChange={e => patchMapping({ batch_property: e.target.value })} aria-label="Batch property" />
          </Field>
          <Field label="Batch size">
            <TextInput type="number" value={String(config.mapping.batch_size ?? 100)}
              onChange={e => patchMapping({ batch_size: Number(e.target.value) || 0 })} aria-label="Batch size" />
          </Field>
        </div>
      )}

      <div style={{ display: "flex", gap: 12, alignItems: "center" }}>
        <Button variant="secondary" onClick={runDry} loading={dryBusy} loadingLabel="Dry run…" aria-busy={dryBusy}>
          Dry-run report
        </Button>
        {dryRun && (
          <span aria-live="polite" style={{ fontSize: 13 }}>
            <StatusBadge tone={dryRun.status === "success" ? "success" : dryRun.status === "partial" ? "warning" : "danger"}>{dryRun.status}</StatusBadge>
            {" "}found {dryRun.records_read}, valid {dryRun.records_written}, skipped {dryRun.records_skipped}
            {dryRun.error_code ? ` · ${dryRun.error_code}` : ""}
          </span>
        )}
      </div>
      {isPull && config.target_type === "grid" && (
        <InlineAlert tone="info">
          Map enough dimensions and metrics to identify a unique grid cell. Values can only be
          written to input metrics; validation, write guards and recalculation reuse the standard
          import pipeline.
        </InlineAlert>
      )}
    </div>
  );
}

function SourceInput({ value, onChange, inferred, label }: {
  value: string; onChange: (v: string) => void; inferred: InferredField[]; label: string;
}) {
  const listID = `paths-${label.replace(/\s+/g, "-")}`;
  return (
    <span style={{ flex: 1, minWidth: 160 }}>
      <TextInput value={value} onChange={e => onChange(e.target.value)} placeholder="$.field" aria-label={label} list={listID} />
      <datalist id={listID}>
        {inferred.map(f => <option key={f.path} value={f.path}>{f.types.join("|")}</option>)}
      </datalist>
    </span>
  );
}

function TransformEditor({ field, onChange, index }: {
  field: ApiFieldMap; onChange: (patch: Partial<ApiFieldMap>) => void; index: number;
}) {
  const t = field.transforms?.[0];
  const kind = t?.kind ?? "";
  const needsValue = kind === "default" || kind === "date_format";
  const needsLookup = kind === "lookup";
  return (
    <span style={{ display: "inline-flex", gap: 4, alignItems: "center" }}>
      <Select value={kind} aria-label={`Mapping ${index + 1} transform`}
        onChange={e => {
          const k = e.target.value;
          onChange({ transforms: k === "" ? [] : [{ kind: k, value: t?.value, lookup: t?.lookup }] });
        }} style={{ width: 140 }}>
        {TRANSFORM_KINDS.map(k => <option key={k.id} value={k.id}>{k.label}</option>)}
      </Select>
      {needsValue && (
        <TextInput value={t?.value ?? ""} onChange={e => onChange({ transforms: [{ kind, value: e.target.value }] })}
          placeholder={kind === "date_format" ? "2006-01-02" : "default"} aria-label={`Mapping ${index + 1} transform value`} style={{ width: 110 }} />
      )}
      {needsLookup && (
        <TextInput value={lookupText(t?.lookup)} aria-label={`Mapping ${index + 1} lookup table`}
          onChange={e => onChange({ transforms: [{ kind, lookup: parseLookup(e.target.value) }] })}
          placeholder="a=b, c=d" style={{ width: 140 }} />
      )}
    </span>
  );
}

function lookupText(l?: Record<string, string>): string {
  if (!l) return "";
  return Object.entries(l).map(([k, v]) => `${k}=${v}`).join(", ");
}
function parseLookup(s: string): Record<string, string> {
  const out: Record<string, string> = {};
  for (const pair of s.split(",")) {
    const [k, ...rest] = pair.split("=");
    if (k.trim() !== "" && rest.length > 0) out[k.trim()] = rest.join("=").trim();
  }
  return out;
}
